package kdlfmt

import "fmt"

// state is the lexical context carried from the end of one line to the
// start of the next: node-nesting depth (for reindenting) plus whichever
// multi-line construct — a block comment, a raw string, or a line-
// continuation — the next line begins inside, if any. The zero value is
// the state at the start of a file: depth 0, nothing open.
type state struct {
	depth int // node-nesting depth the NEXT line starts at

	blockDepth int // >0: next line starts inside a block comment, nested this deep
	blockStart int // line the outermost open block comment began (error messages only)

	rawActive    bool // next line starts inside a multi-line raw string
	rawHashes    int  // '#' count of that raw string's delimiter
	rawSeekClose bool // mid-way through matching the closing quote+hashes
	rawCloseSeen int  // hashes matched so far while rawSeekClose
	rawStart     int  // line the open raw string began (error messages only)

	quotedActive bool // next line starts inside a multi-line quoted string
	quotedStart  int  // line the open quoted string began (error messages only)

	continuation bool // previous line ended with a line-continuation backslash
}

// Lexical sub-modes scanLine can be in partway through a single line. Named
// distinctly from state's fields above: these never survive past the
// return from scanLine except by being folded back into state.
const (
	subNormal = iota
	subBlockComment
	subRawString
	subQuoted
	subLineComment
)

// scanLine scans one line of KDL source, updating the carried lexical
// state and reporting the brace depth this line should be reindented to
// (meaningless for a line the caller is about to pass through untouched —
// see normalize). ln is the 1-indexed line number, used only to name
// malformed input in errors.
//
// It tracks exactly enough to answer "is this `{`/`}` structural" and
// "does this line end mid-construct": line comments (`//` to EOL), block
// comments (`/* */`, which NEST and may span lines — st.blockDepth carries
// the nesting count across the boundary), quoted strings (`"..."`,
// backslash-escaped, must close before EOL), raw strings (`r"..."`,
// `r#"..."#`, ... — arbitrary '#' count, no escapes, MAY span lines exactly
// like a block comment: a raw string's content is never reindented, so a
// literal newline inside one is carried the same way), and `/-` slashdash
// (purely lexical — a bare `/` or `-` is ordinary content here; only `//`
// and `/*` are special, so a slashdashed node's own braces count for depth
// exactly like any other node's, matching the package doc's contract).
//
// A line-continuation backslash (a bare `\` that is the last non-
// whitespace, non-comment character on the line, outside any
// string/comment) marks the NEXT line for passthrough — continuation
// alignment is the author's, not this formatter's.
//
// Malformed input refuses rather than guessing: an unterminated quoted
// string at EOL, or more closing braces than open (negative depth), each
// return an error naming ln. An unterminated block comment or raw string
// at EOF is instead reported by normalize once scanning finishes, since
// only it knows there is no next line to carry the state onto.
func scanLine(line []byte, ln int, st state) (lineDepth int, next state, err error) {
	depth := st.depth

	sub := subNormal
	blockDepth := st.blockDepth
	blockStart := st.blockStart
	rawHashes := st.rawHashes
	rawSeekClose := st.rawSeekClose
	rawCloseSeen := st.rawCloseSeen
	rawStart := st.rawStart
	quotedStart := st.quotedStart
	switch {
	case blockDepth > 0:
		sub = subBlockComment
	case st.rawActive:
		sub = subRawString
	case st.quotedActive:
		// A quoted string left open at the previous EOL: the newline was
		// string content (or was consumed by a trailing backslash's
		// whitespace escape — either way the escape ended with the line, so
		// this line starts unescaped).
		sub = subQuoted
	}

	// leadingRun tracks the "first token(s) are closing braces" run
	// (transform 1's dedent rule): true only while every character seen so
	// far on this line has been whitespace or a structural '}'. It starts
	// false when the line begins mid block-comment/raw-string, since such
	// a line is always passed through untouched regardless of this value.
	leadingRun := sub == subNormal
	leadingCloses := 0
	quotedEscaped := false
	contPending := false

	i, n := 0, len(line)
scan:
	for i < n {
		c := line[i]
		switch sub {
		case subBlockComment:
			switch {
			case c == '*' && i+1 < n && line[i+1] == '/':
				blockDepth--
				i += 2
				if blockDepth == 0 {
					sub = subNormal
				}
			case c == '/' && i+1 < n && line[i+1] == '*':
				blockDepth++
				i += 2
			default:
				i++
			}

		case subRawString:
			if rawSeekClose {
				switch c {
				case '#':
					rawCloseSeen++
					i++
					if rawCloseSeen == rawHashes {
						sub = subNormal
						rawSeekClose = false
					}
					continue scan
				case '"':
					// Another quote before enough '#' matched: the close
					// attempt restarts from THIS quote (mirrors kdl-go's
					// own raw-string reader).
					rawCloseSeen = 0
					i++
					continue scan
				default:
					rawSeekClose = false
					rawCloseSeen = 0
					// c is ordinary raw-string content; fall through.
				}
			}
			if c == '"' {
				rawSeekClose = true
				rawCloseSeen = 0
				if rawHashes == 0 {
					sub = subNormal
					rawSeekClose = false
				}
			}
			i++

		case subQuoted:
			switch {
			case quotedEscaped:
				quotedEscaped = false
			case c == '\\':
				quotedEscaped = true
			case c == '"':
				sub = subNormal
			}
			i++

		case subNormal:
			switch {
			case c == '"':
				sub = subQuoted
				quotedEscaped = false
				quotedStart = ln
				leadingRun = false
				contPending = false
				i++
			case c == 'r' && rawStringStart(line, i):
				hashes := 0
				j := i + 1
				for j < n && line[j] == '#' {
					hashes++
					j++
				}
				// rawStringStart already confirmed line[j] == '"'.
				sub = subRawString
				rawHashes = hashes
				rawSeekClose = false
				rawCloseSeen = 0
				rawStart = ln
				leadingRun = false
				contPending = false
				i = j + 1
			case c == '/' && i+1 < n && line[i+1] == '/':
				leadingRun = false
				// contPending deliberately survives: KDL allows a line
				// comment after a continuation backslash (`node \ // note`),
				// and the continuation still stands.
				break scan // rest of line is comment text; nothing more to track
			case c == '/' && i+1 < n && line[i+1] == '*':
				sub = subBlockComment
				blockDepth = 1
				blockStart = ln
				leadingRun = false
				contPending = false
				i += 2
			case c == '{':
				depth++
				leadingRun = false
				contPending = false
				i++
			case c == '}':
				if leadingRun {
					leadingCloses++
				}
				depth--
				if depth < 0 {
					return 0, state{}, fmt.Errorf("kdlfmt: line %d: unmatched '}' (more closing braces than open)", ln)
				}
				contPending = false
				i++
			case c == ' ' || c == '\t' || c == '\r':
				i++ // whitespace never ends a leading-close run or a pending continuation
			case c == '\\':
				contPending = true
				leadingRun = false
				i++
			default:
				leadingRun = false
				contPending = false
				i++
			}
		}
	}

	next = state{
		depth:        depth,
		blockDepth:   blockDepth,
		blockStart:   blockStart,
		rawActive:    sub == subRawString,
		rawHashes:    rawHashes,
		rawSeekClose: rawSeekClose,
		rawCloseSeen: rawCloseSeen,
		rawStart:     rawStart,
		// A quoted string still open at EOL is a KDL multi-line string
		// (plain newline-spanning, `"""` triple-quoted — which lexes here
		// as empty-string, open, ..., close, empty-string — or a trailing
		// backslash whitespace-escape): its interior lines pass through
		// untouched, exactly like a raw string's. A genuinely unterminated
		// string swallows the rest of the file and is refused at EOF by
		// normalize (or by Format's input parse guard before that).
		quotedActive: sub == subQuoted,
		quotedStart:  quotedStart,
		// A continuation only carries when the line ended lexically
		// normal: a line that ends mid comment/string is already forcing
		// the next line to pass through via blockDepth/rawActive/
		// quotedActive, and a trailing backslash inside either is just
		// content, not a continuation marker.
		continuation: sub == subNormal && contPending,
	}
	return st.depth - leadingCloses, next, nil
}

// rawStringStart reports whether line[i] ('r') begins a raw-string token
// r#*" rather than continuing a bareword like "run" or "for": true when a
// (possibly empty) run of '#' immediately follows the 'r' and is itself
// immediately followed by '"', AND the 'r' itself sits at a word boundary
// (not preceded by an identifier-continuing character, which would make it
// the middle of some other bareword rather than a new token's start).
func rawStringStart(line []byte, i int) bool {
	if i > 0 {
		switch line[i-1] {
		case ' ', '\t', '\r', '{', '}', '(', ')', ';', '=', '"', '-':
			// boundary: 'r' may start a new token here. '-' is the
			// slashdash case: `/-r#"..."#` puts a raw string directly
			// after the dash, and missing it would re-lex the raw
			// content as barewords and quoted strings — any brace in
			// there would then miscount structural depth.
		default:
			return false
		}
	}
	j := i + 1
	for j < len(line) && line[j] == '#' {
		j++
	}
	return j < len(line) && line[j] == '"'
}
