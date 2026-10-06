package dashboard

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/queue"
)

var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

func targetRuns(target queue.TargetSnapshot) []queue.RunSnapshot {
	if len(target.Pipeline) != 0 {
		return target.Pipeline
	}
	if target.InFlight != nil {
		return []queue.RunSnapshot{*target.InFlight}
	}
	return nil
}

func runMembers(run queue.RunSnapshot) []core.Candidate {
	if len(run.Members) != 0 {
		return run.Members
	}
	return []core.Candidate{run.Candidate}
}

func (d *dash) handleCommit(w http.ResponseWriter, r *http.Request) {
	sha := strings.ToLower(r.PathValue("sha"))
	if !commitSHA.MatchString(sha) {
		http.Error(w, "use a full commit SHA", http.StatusBadRequest)
		return
	}
	d.progress(w, r, sha, r.URL.Query().Get("target"), r.URL.Query().Get("ref"))
}

func (d *dash) handleChange(w http.ResponseWriter, r *http.Request) {
	target, ref := r.URL.Query().Get("target"), r.URL.Query().Get("ref")
	if target == "" || ref == "" {
		http.Error(w, "target and ref are required", http.StatusBadRequest)
		return
	}
	d.progress(w, r, "", target, ref)
}

func (d *dash) progress(w http.ResponseWriter, r *http.Request, sha, target, ref string) {
	w.Header().Set("Cache-Control", "no-store")
	matches := func(c core.Candidate) bool {
		return (target == "" || c.Target == target) && (ref == "" || c.Ref == ref)
	}
	snap := d.snapshot()
	label, identity := "Commit", sha
	if sha == "" {
		label, identity = "Change", ref
	}
	data := progressData{baseData: d.newBase(label+" progress", snap, true, "targets"), Label: label, Identity: identity,
		Detail: "No run yet. The request may be awaiting admission, withdrawn, or not known to this daemon."}
	if snap != nil {
		for _, t := range snap.Targets {
			if target != "" && t.Name != target {
				continue
			}
			for _, run := range targetRuns(t) {
				for _, c := range runMembers(run) {
					if matches(c) && (sha == "" || c.SHA == sha || run.MergeSHA == sha || run.ChainTip == sha) {
						d.openProgressRun(w, r, run.RunID, sha == "")
						return
					}
				}
			}
			for _, entry := range t.Waiting {
				if matches(entry.Candidate) && (sha == "" || entry.Candidate.SHA == sha) {
					data.Target, data.Detail = t.Name, "Queued; waiting for a verification slot."
					if t.Pause != nil {
						data.Detail = "Queued; target is paused: " + t.Pause.Reason
					}
					if t.Circuit != nil {
						data.Detail = "Queued; infrastructure circuit is open."
					}
					render(w, progressTmpl, data)
					return
				}
			}
			for _, entry := range t.Parked {
				if matches(entry.Candidate) && (sha == "" || entry.Candidate.SHA == sha) {
					data.Target, data.Detail = t.Name, "Parked: "+entry.Reason
				}
			}
		}
	}
	if d.store != nil {
		var row history.RunRow
		var err error
		if sha == "" {
			row, err = d.store.RunForChange(target, ref)
		} else {
			row, err = d.store.RunForCommit(sha, target, ref)
		}
		if err == nil {
			d.openProgressRun(w, r, row.RunID, sha == "")
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("dashboard: progress lookup: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	} else {
		data.Detail += " History is disabled; completed attempts are not retained."
	}
	render(w, progressTmpl, data)
}

func (d *dash) openProgressRun(w http.ResponseWriter, r *http.Request, id string, follow bool) {
	if follow {
		d.renderRun(w, r, id, true)
	} else {
		http.Redirect(w, r, "/run/"+url.PathEscape(id), http.StatusFound)
	}
}

func (d *dash) liveRun(w http.ResponseWriter, id string, follow bool) bool {
	snap := d.snapshot()
	if snap == nil {
		return false
	}
	for _, t := range snap.Targets {
		for _, run := range targetRuns(t) {
			if run.RunID != id {
				continue
			}
			c := run.Candidate
			w.Header().Set("Cache-Control", "no-store")
			data := runData{baseData: d.newBase("Run", snap, true, "targets"), StoreEnabled: true, Live: true, FollowingChange: follow,
				Run: runSummaryFull{RunID: id, Target: t.Name, Ref: c.Ref, SHA: c.SHA, User: c.User, Topic: c.Topic,
					BaseOID: run.BaseOID, MergeSHA: run.MergeSHA, TrialClean: true, Outcome: wordTag("running"),
					Detail: "Verifying the proposed target history.", StartedAt: formatTime(run.StartedAt),
					Duration: formatDuration(snap.At.Sub(run.StartedAt)), Speculated: run.Predicted}}
			for _, member := range runMembers(run) {
				data.Members = append(data.Members, memberView{User: member.User, Topic: member.Topic, SHA: member.SHA})
			}
			for _, check := range run.Done {
				data.Checks = append(data.Checks, checkView{Name: check.Name, Status: checkTag(check.Status),
					RowID: check.Name, Output: check.Output, Err: errText(check.Err), Duration: formatDuration(check.Duration),
					Open: check.Status == core.CheckFailed || check.Err != nil})
			}
			running := run.Running
			if len(running) == 0 && run.Current != nil {
				running = []queue.CurrentCheck{*run.Current}
			}
			for _, check := range running {
				data.Checks = append(data.Checks, checkView{Name: check.Name, RowID: check.Name, Status: wordTag("running"),
					Duration: formatDuration(snap.At.Sub(check.StartedAt))})
			}
			render(w, runTmpl, data)
			return true
		}
	}
	return false
}

type progressData struct {
	baseData
	Label, Identity, Target, Detail string
}
