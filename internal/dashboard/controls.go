package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/queue"
)

type emergencyChoice struct{ Label, Revisions string }

// Each choice is an explicitly requested prefix, with parents before children.
func emergencyChoices(target queue.TargetSnapshot) []emergencyChoice {
	var pending []core.Candidate
	for _, run := range target.Pipeline {
		pending = append(pending, run.Members...)
	}
	if len(target.Pipeline) == 0 && target.InFlight != nil {
		pending = append(pending, target.InFlight.Members...)
		if len(target.InFlight.Members) == 0 {
			pending = append(pending, target.InFlight.Candidate)
		}
	}
	for _, w := range target.Waiting {
		pending = append(pending, w.Candidate)
	}
	for _, p := range target.Parked {
		pending = append(pending, p.Candidate)
	}
	var revisions []core.Revision
	var out []emergencyChoice
	seen := map[string]bool{}
	for len(pending) > 0 {
		progress := false
		for i, c := range pending {
			if c.Ref == "" || seen[c.Ref] {
				pending = append(pending[:i], pending[i+1:]...)
				progress = true
				break
			}
			if c.DependsOn != "" && !seen[c.DependsOn] {
				continue
			}
			seen[c.Ref] = true
			revisions = append(revisions, core.Revision{Ref: c.Ref, SHA: c.SHA, Version: c.Version})
			data, _ := json.Marshal(revisions)
			out = append(out, emergencyChoice{fmt.Sprintf("Through %s/%s (%d changes)", c.User, c.Topic, len(revisions)), string(data)})
			pending = append(pending[:i], pending[i+1:]...)
			progress = true
			break
		}
		if !progress {
			break
		}
	}
	return out
}

func (d *dash) handleAPIControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, 405, "POST required")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			writeJSONError(w, 403, "cross-origin control rejected")
			return
		}
	}
	if d.ch == nil {
		writeJSONError(w, 503, "queue controls unavailable")
		return
	}
	var cmd core.Command
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cmd); err != nil {
		writeJSONError(w, 400, "invalid control request")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeJSONError(w, 400, "one control request required")
		return
	}
	if cmd.RequestID != "" {
		writeJSONError(w, 400, "request identity is reserved for forge commands")
		return
	}
	switch cmd.Kind {
	case core.CommandPause, core.CommandResume, core.CommandUrgent, core.CommandMergeAnyway, core.CommandMergePaused:
	default:
		writeJSONError(w, 400, "unknown control")
		return
	}
	if strings.TrimSpace(cmd.Reason) == "" || len(cmd.Reason) > 2048 {
		writeJSONError(w, 400, "reason required (max 2048 bytes)")
		return
	}
	if cmd.Actor == "" {
		cmd.Actor = r.RemoteAddr
	} else {
		cmd.Actor += " (" + r.RemoteAddr + ")"
	}
	if cmd.Kind == core.CommandMergePaused && !cmd.OverridePause {
		writeJSONError(w, 400, "explicit pause override required")
		return
	}
	snap := d.snapshot()
	if snap == nil {
		writeJSONError(w, 503, "queue starting")
		return
	}
	if cmd.Target == "*" && (cmd.Kind == core.CommandPause || cmd.Kind == core.CommandResume) {
		if !d.ch.TrySend(cmd) {
			writeJSONError(w, 429, "command buffer full")
			return
		}
		writeJSON(w, 202, map[string]string{"status": "queued"})
		return
	}
	target, ok := findTarget(snap, cmd.Target)
	if !ok {
		writeJSONError(w, 400, "unknown target")
		return
	}
	if (cmd.Kind == core.CommandMergeAnyway || cmd.Kind == core.CommandMergePaused) && !target.EmergencyEnabled {
		writeJSONError(w, 403, "emergency merging is not enabled")
		return
	}
	if (cmd.Kind == core.CommandMergeAnyway || cmd.Kind == core.CommandMergePaused) && target.Pause != nil && !cmd.OverridePause {
		writeJSONError(w, 409, "explicit pause override required")
		return
	}
	if cmd.Kind == core.CommandUrgent || (cmd.Kind == core.CommandMergeAnyway || cmd.Kind == core.CommandMergePaused) {
		choices := emergencyChoices(target)
		if len(cmd.Revisions) == 0 {
			writeJSONError(w, 400, "exact revisions required")
			return
		}
		valid := false
		for _, choice := range choices {
			var members []core.Revision
			_ = json.Unmarshal([]byte(choice.Revisions), &members)
			for _, rev := range members {
				if len(cmd.Revisions) == 1 && cmd.Revisions[0] == rev {
					valid = true
				}
			}
			data, _ := json.Marshal(cmd.Revisions)
			if string(data) == choice.Revisions {
				valid = true
			}
		}
		if !valid {
			writeJSONError(w, 409, "selected revisions changed; refresh and select again")
			return
		}
	}
	if !d.ch.TrySend(cmd) {
		writeJSONError(w, 429, "command buffer full")
		return
	}
	writeJSON(w, 202, map[string]string{"status": "queued; check target status for acknowledgement"})
}
