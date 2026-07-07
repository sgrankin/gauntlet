// `gauntlet land` is client-side porcelain: it pushes the current HEAD to a
// candidate ref (the core.Candidate grammar)
// "for/<target>/<user>/<topic>". It does no queue logic of its own — the
// daemon does the actual trial-merge/test/land — it just saves typing the
// refspec by hand.
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
	target := fs.String("target", "", "target name, matching a `target` in the daemon's gauntlet.kdl; defaults to the remote's default branch")
	topic := fs.String("topic", "", "topic name; defaults to the current branch, or to the one local branch at HEAD")
	remote := fs.String("remote", "origin", "git remote to push to")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tgt := *target
	if tgt == "" {
		var err error
		tgt, err = inferTarget(*remote)
		if err != nil {
			return err
		}
	}

	t := *topic
	if t == "" {
		var err error
		t, err = inferTopic(tgt)
		if err != nil {
			return err
		}
	}

	name := ""
	if out, err := exec.Command("git", "config", "user.name").Output(); err == nil {
		name = strings.TrimSpace(string(out))
	}
	if name == "" {
		name = os.Getenv("USER")
	}

	refspec := landRefspec(tgt, slugifyUser(name), t)
	cmd := exec.Command("git", "push", *remote, refspec)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// inferTarget resolves the remote's default branch from the local
// refs/remotes/<remote>/HEAD symref (set by clone, or by `git remote
// set-head <remote> --auto`). A daemon target is conventionally named after
// its branch; config lets the two differ (a slashed branch requires it), so
// inference only covers the matching case and anything else takes -target.
func inferTarget(remote string) (string, error) {
	out, err := exec.Command("git", "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("-target not given and the remote's default branch is unknown (git symbolic-ref refs/remotes/%s/HEAD: %v); pass -target, or set the symref once with `git remote set-head %s --auto`", remote, err, remote)
	}
	return targetFromRemoteHead(strings.TrimSpace(string(out)), remote)
}

// targetFromRemoteHead extracts the target name from `git symbolic-ref
// --short refs/remotes/<remote>/HEAD` output: "origin/main" -> "main". A
// slashed branch name is an error, not a target: target names must not
// contain '/' (the queue grammar takes the first segment after for/ as the
// target), so guessing here would push a well-formed ref aimed at the
// wrong queue.
func targetFromRemoteHead(symref, remote string) (string, error) {
	branch, ok := strings.CutPrefix(symref, remote+"/")
	if !ok || branch == "" {
		return "", fmt.Errorf("unexpected refs/remotes/%s/HEAD value %q", remote, symref)
	}
	if strings.Contains(branch, "/") {
		return "", fmt.Errorf("the remote's default branch %q can't name a target (target names contain no '/'); pass -target", branch)
	}
	return branch, nil
}

// inferTopic names the topic from where HEAD sits: the checked-out branch,
// or — on a detached HEAD, the normal state of a colocated jj repo, which
// exports local bookmarks as git branches — the one local branch pointing
// at HEAD. The target's own branch never names a topic (see topicFromRefs).
func inferTopic(target string) (string, error) {
	if out, err := exec.Command("git", "symbolic-ref", "--short", "HEAD").Output(); err == nil {
		branch := strings.TrimSpace(string(out))
		if branch == target {
			return "", fmt.Errorf("-topic not given and the checked-out branch is the target %q itself; pass -topic", target)
		}
		return branch, nil
	}
	// Full refnames, not %(refname:short): shortening is a display
	// heuristic that disambiguates shadowed names (a local branch
	// "origin/main" prints as "heads/origin/main"), which would leak into
	// the pushed ref.
	out, err := exec.Command("git", "for-each-ref", "--format=%(refname)", "--points-at", "HEAD", "refs/heads/").Output()
	if err != nil {
		return "", fmt.Errorf("-topic not given and HEAD isn't on a branch: %w", err)
	}
	return topicFromRefs(strings.Fields(string(out)), target)
}

// topicFromRefs picks the topic from the local branches at a detached HEAD,
// given as full refnames: exactly one is unambiguous, anything else needs
// -topic. The target's own branch is excluded before counting — right after
// checking out the target tip (colocated jj: right after `jj new main`) it
// points at HEAD, and inferring it would push the target's own tip as a
// candidate instead of erroring.
func topicFromRefs(refs []string, target string) (string, error) {
	var branches []string
	for _, ref := range refs {
		branch, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok || branch == target {
			continue
		}
		branches = append(branches, branch)
	}
	switch len(branches) {
	case 0:
		return "", fmt.Errorf("-topic not given, HEAD isn't on a branch, and no local branch (other than the target's) points at HEAD")
	case 1:
		return branches[0], nil
	default:
		return "", fmt.Errorf("-topic not given and %d local branches point at HEAD (%s)", len(branches), strings.Join(branches, ", "))
	}
}

// landRefspec builds the "git push <remote> <refspec>" destination for a
// candidate, matching core.Candidate's ref grammar.
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
