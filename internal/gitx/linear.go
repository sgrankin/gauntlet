package gitx

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
)

var _ core.LinearGitRepo = (*Repo)(nil)

// FetchReview imports and anchors a forge's review ref, including fork PRs.
// It never changes a remote ref or the ordinary queue's fetched namespace.
func (r *Repo) FetchReview(ctx context.Context, remoteRef, localRef, expected string) error {
	if _, err := r.runRemote(ctx, "fetch", "--no-tags", "origin", "+"+remoteRef+":"+localRef); err != nil {
		return fmt.Errorf("fetch review: %w", err)
	}
	actual, err := r.run(ctx, "rev-parse", localRef)
	if err != nil || strings.TrimSpace(actual) != expected {
		return fmt.Errorf("review moved while fetching")
	}
	return r.touchSource(expected)
}

// ReplayTree applies just sourceBase..candidate onto the predicted target.
// Unlike an ordinary merge, an explicit base remains correct after the
// preceding change in a stack was squashed into a different commit.
func (r *Repo) ReplayTree(ctx context.Context, base, candidate, sourceBase string) (core.TrialMerge, error) {
	if sourceBase == "" {
		return r.MergeTree(ctx, base, candidate)
	}
	if _, err := r.run(ctx, "rev-parse", "--verify", sourceBase+"^{commit}"); err != nil {
		return core.TrialMerge{}, fmt.Errorf("source base: %w", err)
	}
	if ancestor, err := r.IsAncestor(ctx, sourceBase, candidate); err != nil || !ancestor {
		return core.TrialMerge{}, fmt.Errorf("stack base is not an ancestor of its review revision; rebase the stack")
	}
	out, err := r.run(ctx, "merge-tree", "--write-tree", "--merge-base="+sourceBase, base, candidate)
	if err != nil {
		if ge, ok := errors.AsType[*gitError](err); ok && ge.exitCode() == 1 && len(splitLines(out)) > 0 {
			return core.TrialMerge{Conflicts: parseConflictPaths(splitLines(out)[1:])}, nil
		}
		return core.TrialMerge{}, err
	}
	tree, _, _ := strings.Cut(out, "\n")
	return core.TrialMerge{Clean: true, TreeOID: strings.TrimSpace(tree)}, nil
}

// LinearCommit preserves the source author and jj's change-id header. It
// deliberately drops signatures, which are invalid after changing parents
// or the message. Gerrit's Change-Id is part of the message, not this header.
func (r *Repo) LinearCommit(ctx context.Context, tree, base, source, message string, who core.Identity) (string, error) {
	// Single-parent landing commits do not reach their original inputs.
	// Retain those objects locally for audit and delayed post-land hooks,
	// even after contributor refs move. This namespace is never pushed.
	if err := r.retainSource(ctx, source); err != nil {
		return "", fmt.Errorf("retain source: %w", err)
	}
	raw, err := r.run(ctx, "cat-file", "commit", source)
	if err != nil {
		return "", err
	}
	headers, sourceMessage, _ := strings.Cut(raw, "\n\n")
	if message == "" {
		message = sourceMessage
	}
	var author, changeID string
	for line := range strings.SplitSeq(headers, "\n") {
		if strings.HasPrefix(line, "author ") {
			author = line
		}
		if strings.HasPrefix(line, "change-id ") {
			changeID = line
		}
	}
	if author == "" {
		return "", fmt.Errorf("source commit has no author")
	}
	// Ask git to format and validate the new committer identity/date, then
	// write the complete object so the nonstandard jj header survives.
	seed, err := r.CommitTree(ctx, tree, []string{base}, message, who)
	if err != nil {
		return "", err
	}
	seedRaw, err := r.run(ctx, "cat-file", "commit", seed)
	if err != nil {
		return "", err
	}
	seedHeaders, _, _ := strings.Cut(seedRaw, "\n\n")
	var committer string
	for line := range strings.SplitSeq(seedHeaders, "\n") {
		if strings.HasPrefix(line, "committer ") {
			committer = line
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\nparent %s\n%s\n%s\n", tree, base, author, committer)
	if changeID != "" {
		b.WriteString(changeID + "\n")
	}
	b.WriteString("\n" + strings.TrimRight(message, "\n") + "\n")
	out, err := runGit(ctx, r.dir, strings.NewReader(b.String()), "hash-object", "-t", "commit", "-w", "--stdin")
	return strings.TrimSpace(out), err
}

// FindLanding searches only the target's first-parent ledger. A source SHA
// alone is insufficient: the same commit can be submitted to several slots.
func (r *Repo) FindLanding(ctx context.Context, tip, ref, sha, version string) (string, error) {
	out, err := r.run(ctx, "log", "--first-parent", "--fixed-strings", "--grep=Gauntlet-Source: "+sha, "--format=%H%x00%B%x00", tip)
	if err != nil {
		return "", err
	}
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		trailers, err := runGit(ctx, r.dir, strings.NewReader(fields[i+1]), "interpret-trailers", "--parse")
		if err != nil {
			return "", err
		}
		values := map[string]string{}
		for line := range strings.SplitSeq(trailers, "\n") {
			k, v, ok := strings.Cut(line, ": ")
			if ok {
				values[k] = v
			}
		}
		if values["Gauntlet-Ref"] == ref && values["Gauntlet-Source"] == sha && (version == "*" || values["Gauntlet-Version"] == version) {
			return strings.TrimSpace(fields[i]), nil
		}
	}
	return "", nil
}

// ReviewLanded verifies prerequisite landings from target history, never from
// an editable PR comment or a manually closed state.
func (r *Repo) ReviewLanded(ctx context.Context, branch, ref, source string) (bool, error) {
	tip := "refs/remotes/origin/" + branch
	landing, err := r.FindLanding(ctx, tip, ref, source, "*")
	if err != nil || landing != "" {
		return landing != "", err
	}
	return r.IsAncestor(ctx, source, tip)
}

func (r *Repo) ReviewBaseLanded(ctx context.Context, branch, source string) (bool, error) {
	tip := "refs/remotes/origin/" + branch
	if yes, err := r.IsAncestor(ctx, source, tip); err != nil || yes {
		return yes, err
	}
	out, err := r.run(ctx, "log", "--first-parent", "--fixed-strings", "--grep=Gauntlet-Source: "+source, "--format=%B%x00", tip)
	if err != nil {
		return false, err
	}
	for message := range strings.SplitSeq(out, "\x00") {
		trailers, err := runGit(ctx, r.dir, strings.NewReader(message), "interpret-trailers", "--parse")
		if err != nil {
			return false, err
		}
		var value string
		for line := range strings.SplitSeq(trailers, "\n") {
			if v, ok := strings.CutPrefix(line, "Gauntlet-Source: "); ok {
				value = v
			}
		}
		if value == source {
			return true, nil
		}
	}
	return false, nil
}

// CommitMessage returns the source message for an ordinary queue candidate.
func (r *Repo) CommitMessage(ctx context.Context, sha string) (string, error) {
	out, err := r.run(ctx, "show", "-s", "--format=%B", sha)
	return strings.TrimRight(out, "\n"), err
}
