// `gauntlet land` is client-side porcelain: it pushes the current HEAD to a
// candidate ref (the core.Candidate grammar, docs/plans/phase1.md §9.3)
// "for/<target>/<user>/<topic>". It does no queue logic of its own — the
// daemon does the actual trial-merge/test/land — it just saves typing the
// refspec by hand. See docs/plans/phase23.md §6 (chunk D8).
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// runLand implements the "land" subcommand.
func runLand(args []string) error {
	fs := flag.NewFlagSet("land", flag.ExitOnError)
	target := fs.String("target", "", "target name, matching a `target` in the daemon's gauntlet.kdl [required]")
	topic := fs.String("topic", "", "topic name; defaults to the current branch name")
	remote := fs.String("remote", "origin", "git remote to push to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		return fmt.Errorf("-target is required")
	}

	t := *topic
	if t == "" {
		out, err := exec.Command("git", "symbolic-ref", "--short", "HEAD").Output()
		if err != nil {
			return fmt.Errorf("-topic not given and HEAD isn't on a branch: %w", err)
		}
		t = strings.TrimSpace(string(out))
	}

	name := ""
	if out, err := exec.Command("git", "config", "user.name").Output(); err == nil {
		name = strings.TrimSpace(string(out))
	}
	if name == "" {
		name = os.Getenv("USER")
	}

	refspec := landRefspec(*target, slugifyUser(name), t)
	cmd := exec.Command("git", "push", *remote, refspec)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// landRefspec builds the "git push <remote> <refspec>" destination for a
// candidate, per the ref grammar in docs/plans/phase1.md §9.3.
func landRefspec(target, user, topic string) string {
	return fmt.Sprintf("HEAD:refs/heads/for/%s/%s/%s", target, user, topic)
}

var userJunkRE = regexp.MustCompile(`[^a-z0-9-]+`)

// slugifyUser turns a git user.name (or $USER) into a ref-safe path segment:
// lowercased, anything but [a-z0-9-] collapsed to '-', leading/trailing '-'
// trimmed.
func slugifyUser(name string) string {
	return strings.Trim(userJunkRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
}
