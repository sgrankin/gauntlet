// `gauntlet fmt` is client-side porcelain, like validate.go: it never talks
// to git, config semantics, or the queue — only to internal/kdlfmt, the
// line-based KDL whitespace normalizer (see that package's doc for the
// full contract and why it's line-based rather than round-tripping through
// kdl-go's document model). This file is gofmt-shaped CLI wiring only:
// flag parsing, per-file dispatch, and the four output modes.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sgrankin/gauntlet/internal/kdlfmt"
)

// errFmtFailed is returned by runFmtTo when at least one file errored (I/O
// or kdlfmt refused it) or -l found at least one differing file — either
// way, the detail was already written to stderr/stdout by the loop below,
// so main's dispatch must not print it again, only exit 1. Mirrors
// doctor.go's errDoctorFailed.
var errFmtFailed = errors.New("fmt: one or more files failed or differ")

// runFmt implements "gauntlet fmt", writing formatted content/listings/
// diffs to os.Stdout and per-file errors to os.Stderr.
func runFmt(args []string) error {
	return runFmtTo(os.Stdout, os.Stderr, args)
}

// runFmtTo does the actual work. stdout and stderr are split (unlike, say,
// doctor.go's single-writer convention) because stdout here is potentially
// PIPEABLE file content: the default mode with no flags concatenates every
// formatted file to stdout, exactly like `gofmt` with no flags — a
// per-file error interleaved into that same stream would corrupt whatever
// the caller intended to do with the KDL content, so errors always go to
// stderr instead, regardless of mode.
//
// Flag composition mirrors gofmt: -w and -l may combine (write AND list),
// -d combines with either. With none of -w/-l/-d given, every file's
// formatted content is written to stdout in argument order — the
// "diff-free preview" mode. Exit-code contract: 1 if any file couldn't be
// read/formatted, OR (only under -l — the CI mode named in the package's
// task brief) if any file's formatting differs; 0 otherwise. -d and -w
// alone never force exit 1 just because a file changed — writing/diffing a
// change is success, not failure.
func runFmtTo(stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("fmt", flag.ContinueOnError)
	write := fs.Bool("w", false, "write result to (source) file instead of stdout")
	list := fs.Bool("l", false, "list files whose formatting differs from gauntlet fmt's (exit 1 if any — CI mode)")
	diff := fs.Bool("d", false, "display diffs instead of rewriting files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) == 0 {
		return errors.New("no files given: gauntlet fmt has no stdin mode; pass one or more .kdl file paths")
	}

	silent := *write || *list || *diff // suppresses the default stdout content dump
	var anyErr, anyDiff bool

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "gauntlet fmt: %s: %v\n", path, err)
			anyErr = true
			continue
		}

		out, err := kdlfmt.Format(data)
		if err != nil {
			// REFUSE, per internal/kdlfmt's contract: never write/print a
			// partial or truncated result for this file. Other files in
			// the same invocation are unaffected.
			fmt.Fprintf(stderr, "gauntlet fmt: %s: %v\n", path, err)
			anyErr = true
			continue
		}
		changed := !bytes.Equal(data, out)

		if !silent {
			stdout.Write(out)
			continue
		}
		if *list && changed {
			anyDiff = true
			fmt.Fprintln(stdout, path)
		}
		if *diff && changed {
			stdout.Write(unifiedDiff(path, data, out))
		}
		if *write && changed {
			// Only ever written when changed, per the package doc: an
			// already-formatted file is never touched, so its mtime is
			// preserved for free by simply not opening it for write.
			perm := os.FileMode(0o644)
			if info, statErr := os.Stat(path); statErr == nil {
				perm = info.Mode().Perm()
			}
			if err := os.WriteFile(path, out, perm); err != nil {
				fmt.Fprintf(stderr, "gauntlet fmt: %s: write: %v\n", path, err)
				anyErr = true
			}
		}
	}

	if anyErr || anyDiff {
		return errFmtFailed
	}
	return nil
}
