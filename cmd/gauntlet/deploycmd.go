// `gauntlet deploy` and `gauntlet promote` are client-side porcelain in the
// `gauntlet land` mould (cmd/gauntlet/land.go): they push a ref, and that
// is the whole of what they do. Deploying an environment IS moving
// refs/heads/deploy/<env> (internal/deploy's DesiredRefPrefix), so these
// commands never talk to the daemon, never touch the JSON API, and work
// against a remote whose daemon is down. Deployment authority is push
// authority — branch protection on deploy/* is the approval model
// (docs/design/deployment.md, "Approval falls out for free"), and a
// daemon-side "deploy this" endpoint would have quietly routed around it.
//
// One consequence is worth stating plainly, because it is where the shipped
// command differs from the design doc's sketch: a git client cannot read the
// daemon's config, so it cannot know an environment's configured `source`.
// `gauntlet deploy -env prod` on its own therefore has nothing to resolve
// and is an error naming both ways to say what to deploy — -rev (a branch,
// tag, or full SHA, resolved on the REMOTE) or -from-env (another
// environment's observed ref, i.e. what that environment has actually
// finished deploying). `gauntlet promote -from dev -to prod` is sugar for
// the second form and shares its implementation exactly.
//
// The push is a compare-and-swap, using the same `--force-with-lease=<ref>:
// <old>` primitive (and the same "stale info" reading) the daemon's own
// gitx.CASUpdate uses: the value read by ls-remote a moment ago is the
// lease, so two people deploying different revisions at once cannot silently
// lose one of them — the loser is told the ref moved and re-runs.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/sgrankin/gauntlet/internal/deploy"
)

// deployOptions is the resolved argument set both subcommands run on:
// `promote` is exactly `deploy` with fromEnv set, so it parses into this
// same struct rather than growing a second code path.
type deployOptions struct {
	env     string // environment to deploy: names refs/heads/deploy/<env>
	rev     string // branch, tag, or full SHA; mutually exclusive with fromEnv
	fromEnv string // promote source: reads refs/gauntlet/deployed/<fromEnv>
	remote  string // git remote (name or URL/path), default "origin"
}

// parseDeployFlags parses "gauntlet deploy"'s flags. flag.ContinueOnError
// (rather than land.go's ExitOnError) so tests can exercise the bad-flag
// paths without exiting the test binary — the same choice status.go makes;
// main's dispatch turns the error into the usual "print to stderr, exit 1".
func parseDeployFlags(args []string) (deployOptions, error) {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	var o deployOptions
	fs.StringVar(&o.env, "env", "", "environment to deploy, matching an `environment` in the daemon's gauntlet.kdl [required]")
	fs.StringVar(&o.rev, "rev", "", "revision to deploy: a branch or tag resolved on the remote, or a full SHA")
	fs.StringVar(&o.fromEnv, "from-env", "", "promote what this environment has finished deploying (its refs/gauntlet/deployed/<env>); mutually exclusive with -rev")
	fs.StringVar(&o.remote, "remote", "origin", "git remote to push to")
	if err := fs.Parse(args); err != nil {
		return deployOptions{}, err
	}
	return o, o.validate()
}

// parsePromoteFlags parses "gauntlet promote"'s flags into the same options
// `deploy` runs on: promote -from dev -to prod IS deploy -env prod -from-env
// dev. Keeping it a spelling rather than a second implementation is the
// point — there is exactly one CAS-push path to get wrong.
func parsePromoteFlags(args []string) (deployOptions, error) {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	var o deployOptions
	fs.StringVar(&o.fromEnv, "from", "", "environment to promote FROM; its refs/gauntlet/deployed/<env> is the revision [required]")
	fs.StringVar(&o.env, "to", "", "environment to promote TO [required]")
	fs.StringVar(&o.remote, "remote", "origin", "git remote to push to")
	if err := fs.Parse(args); err != nil {
		return deployOptions{}, err
	}
	if o.fromEnv == "" {
		return deployOptions{}, errors.New("-from is required (the environment whose deployed revision to promote)")
	}
	if o.env == "" {
		return deployOptions{}, errors.New("-to is required (the environment to promote into)")
	}
	return o, o.validate()
}

// validate rejects the flag combinations that have no meaning, including the
// one the design doc's sketch allowed and this implementation cannot: with
// no daemon round-trip there is no way to learn the environment's configured
// source, so "deploy the source tip" is not a thing the CLI can do. The
// error says so and names both flags rather than leaving the reader to guess
// which one they wanted.
func (o deployOptions) validate() error {
	if o.env == "" {
		return errors.New("-env is required (the environment to deploy)")
	}
	if o.rev != "" && o.fromEnv != "" {
		return errors.New("-rev and -from-env are mutually exclusive: name a revision, or promote another environment's deployed revision")
	}
	if o.rev == "" && o.fromEnv == "" {
		return fmt.Errorf("-rev or -from-env is required: this command pushes %s%s directly and never asks the daemon anything, so it cannot read the environment's configured source — name the revision with -rev (a branch, a tag, or a full SHA), or promote another environment's deployed revision with -from-env", deploy.DesiredRefPrefix, o.env)
	}
	if o.remote == "" {
		return errors.New("-remote must not be empty")
	}
	return nil
}

// runDeploy implements the "deploy" subcommand.
func runDeploy(args []string) error {
	o, err := parseDeployFlags(args)
	if err != nil {
		return err
	}
	return deployEnv(os.Stdout, o)
}

// runPromote implements the "promote" subcommand: same work, friendlier
// spelling for the one case that reads badly as a -from-env flag.
func runPromote(args []string) error {
	o, err := parsePromoteFlags(args)
	if err != nil {
		return err
	}
	return deployEnv(os.Stdout, o)
}

// deployEnv is the whole command: resolve what to deploy on the remote, make
// sure the local repo can name that object, then CAS-push the environment's
// desired ref to it.
//
// Resolution happens against the REMOTE, never the local clone, on purpose:
// "deploy main" means the main the remote has, not whatever a stale local
// checkout thinks main is. The local repo is only ever a vehicle for the
// push itself.
func deployEnv(out io.Writer, o deployOptions) error {
	sha, srcRef, err := resolveDeployRev(o)
	if err != nil {
		return err
	}
	if err := ensureLocalObject(o.remote, sha, srcRef); err != nil {
		return err
	}

	ref := deploy.DesiredRef(o.env)
	old, err := lsRemoteExact(o.remote, ref)
	if err != nil {
		return err
	}
	if old == sha {
		// Not an error: re-pushing the same value is what a re-deploy
		// request looks like, and the daemon is level-triggered, so the push
		// would be a no-op the operator could easily read as "it worked".
		// Say what actually happened instead. Re-running the graph for a
		// revision an environment is already pointed at is the retry surface's
		// job (POST /api/v1/deploy/retry, the MCP deploy_retry tool, or the
		// dashboard button) — there is no ref push that expresses it.
		fmt.Fprintf(out, "%s already at %s; nothing to push (use the retry surface to re-run the graph)\n", ref, sha)
		return nil
	}
	if err := casPushDeployRef(o.remote, ref, old, sha); err != nil {
		return err
	}

	from := ""
	if srcRef != "" {
		from = " (" + srcRef + ")"
	}
	fmt.Fprintf(out, "%s: %s -> %s%s\n", ref, orNew(old), sha, from)
	return nil
}

// resolveDeployRev turns the flags into a concrete SHA plus, when there was
// one, the remote ref it came from (used both for the printed line and as
// the refspec to fetch if the object isn't local yet).
func resolveDeployRev(o deployOptions) (sha, srcRef string, err error) {
	if o.fromEnv != "" {
		ref := deploy.ObservedRef(o.fromEnv)
		sha, err := lsRemoteExact(o.remote, ref)
		if err != nil {
			return "", "", err
		}
		if sha == "" {
			return "", "", fmt.Errorf("-from-env %q: %s does not exist on %s — that environment has never finished a deploy (the daemon writes its observed ref only after an all-green graph run)", o.fromEnv, ref, o.remote)
		}
		return sha, ref, nil
	}
	if fullSHARE.MatchString(o.rev) {
		// A full object name is taken verbatim: it names one revision
		// unambiguously, so asking the remote to confirm it buys nothing the
		// push itself won't say more clearly.
		return o.rev, "", nil
	}
	return lsRemoteRev(o.remote, o.rev)
}

// fullSHARE matches a full object name (sha1 or sha256), the one -rev form
// resolved without asking the remote anything.
var fullSHARE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// lsRemoteRev resolves a branch or tag name to the tip the REMOTE currently
// has. Ambiguity (a branch and a tag of the same name, say) is an error
// rather than a guess: picking one would deploy a revision the operator
// didn't name.
func lsRemoteRev(remote, rev string) (sha, srcRef string, err error) {
	// Both patterns: ls-remote matches whole path components from the tail,
	// so the pattern that finds "refs/tags/v1" does NOT also find that tag's
	// peeled "refs/tags/v1^{}" entry — asking for it explicitly is what makes
	// the peeling below reachable for an annotated tag.
	refs, err := lsRemote(remote, rev, rev+"^{}")
	if err != nil {
		return "", "", err
	}
	switch len(refs) {
	case 0:
		return "", "", fmt.Errorf("-rev %q: no branch or tag on %s matches (push it first, or pass a full SHA)", rev, remote)
	case 1:
		for name, oid := range refs {
			return oid, name, nil
		}
	}
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return "", "", fmt.Errorf("-rev %q is ambiguous on %s: it matches %s; pass the full ref name", rev, remote, strings.Join(names, ", "))
}

// lsRemoteExact resolves one fully-qualified ref, returning "" when the
// remote doesn't have it. Only an exact name match counts: ls-remote's
// pattern matching is tail-anchored, so a longer ref ending in the same path
// components would otherwise answer for a ref that doesn't exist.
func lsRemoteExact(remote, ref string) (string, error) {
	refs, err := lsRemote(remote, ref)
	if err != nil {
		return "", err
	}
	return refs[ref], nil
}

// lsRemote runs `git ls-remote <remote> <patterns...>` and returns the
// matching refs by name. An annotated tag's peeled entry ("refs/tags/v1^{}")
// replaces its own tag object's entry, so deploying a tag deploys the commit
// it points at rather than the tag object — the only kind of object a branch
// ref may hold, and the only one a deploy means anything for.
func lsRemote(remote string, patterns ...string) (map[string]string, error) {
	cmd := exec.Command("git", append([]string{"ls-remote", remote}, patterns...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git ls-remote %s %s: %v: %s", remote, strings.Join(patterns, " "), err, strings.TrimSpace(stderr.String()))
	}
	refs := make(map[string]string)
	peeled := make(map[string]string)
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		oid, name := fields[0], fields[1]
		if base, ok := strings.CutSuffix(name, "^{}"); ok {
			peeled[base] = oid
			continue
		}
		refs[name] = oid
	}
	maps.Copy(refs, peeled)
	return refs, nil
}

// ensureLocalObject makes sure the local repository can name sha, because
// `git push <sha>:<ref>` resolves its source locally — the remote already
// having the object is not enough. This is the common case rather than an
// edge one: a normal clone never fetches refs/gauntlet/deployed/* at all, so
// every promotion needs this, as does deploying a branch tip the local clone
// hasn't seen yet.
func ensureLocalObject(remote, sha, srcRef string) error {
	if haveObject(sha) {
		return nil
	}
	spec := srcRef
	if spec == "" {
		spec = sha
	}
	cmd := exec.Command("git", "fetch", "--no-tags", "--quiet", remote, spec)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("revision %s isn't in this repository and fetching %s from %s failed: %v: %s", sha, spec, remote, err, strings.TrimSpace(stderr.String()))
	}
	if !haveObject(sha) {
		return fmt.Errorf("revision %s isn't in this repository and fetching %s from %s didn't bring it in", sha, spec, remote)
	}
	return nil
}

func haveObject(sha string) bool {
	return exec.Command("git", "cat-file", "-e", sha+"^{commit}").Run() == nil
}

// casPushDeployRef pushes sha to ref under a lease on old ("" meaning "the
// ref must not exist yet"), exactly as gitx.CASUpdate does for the daemon's
// own refs — including reading git's "stale info" as the lost-race signal.
// A lost lease is a plain re-run, not a conflict to resolve: whoever moved
// the ref pushed a revision this command hasn't looked at, so the right
// thing is to look again.
func casPushDeployRef(remote, ref, old, sha string) error {
	lease := "--force-with-lease=" + ref + ":" + old
	cmd := exec.Command("git", "push", lease, remote, sha+":"+ref)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "stale info") {
			return fmt.Errorf("%s on %s moved since it was read (expected %s): ref moved, re-run to retry against the new value", ref, remote, orNew(old))
		}
		return fmt.Errorf("git push %s: %v: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// orNew renders a leased old value for humans: an absent ref is "(new)",
// which is what distinguishes a first deploy from an advance in both the
// success line and the lost-lease error.
func orNew(old string) string {
	if old == "" {
		return "(new)"
	}
	return shortSHA(old)
}
