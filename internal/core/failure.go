package core

import "strings"

// FirstFailure returns the first check in r.Checks whose Status is
// CheckFailed or whose Err is set — the one whose output a terminal
// notification should surface (Watch item, DESIGN.md: "Red pings need the
// failing output"). It returns nil if every check passed or was skipped.
func (r *RunRecord) FirstFailure() *CheckResult {
	for i := range r.Checks {
		cr := &r.Checks[i]
		if cr.Status == CheckFailed || cr.Err != nil {
			return cr
		}
	}
	return nil
}

// FailureTail renders the human-useful tail of a failing check's output: the
// last maxLines non-empty lines of res.Output, further capped at maxBytes
// bytes (truncating from the front, keeping the end — the failure is almost
// always in the last lines). When res.Err is set, its message is appended as
// a final line, since Err is the daemon-caused failure a check's own Output
// never mentions.
//
// FailureTail returns "" for a nil res, and never panics on an empty or
// all-whitespace Output.
func FailureTail(res *CheckResult, maxLines, maxBytes int) string {
	if res == nil {
		return ""
	}

	var lines []string
	for _, ln := range strings.Split(res.Output, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		lines = append(lines, ln)
	}
	if res.Err != nil {
		lines = append(lines, res.Err.Error())
	}
	if len(lines) == 0 {
		return ""
	}

	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out := strings.Join(lines, "\n")

	if maxBytes > 0 && len(out) > maxBytes {
		out = out[len(out)-maxBytes:]
		// Trim a partial first line left over from the byte cut, so the
		// result starts cleanly at a line boundary rather than mid-word.
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}

	return out
}
