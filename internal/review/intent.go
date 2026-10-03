package review

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/statefile"
)

type parsedCommand struct {
	Action                            string
	Count                             int
	Urgent, SkipChecks, OverridePause bool
	Reason                            string
}

func parseCommand(body, bot string) (parsedCommand, bool) {
	var out parsedCommand
	line, reason, hasReason := strings.Cut(strings.TrimSpace(body), " -- ")
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "@"+bot {
		return out, false
	}
	rest := []string{fields[0], fields[1]}
	seen := map[string]bool{}
	for _, field := range fields[2:] {
		switch field {
		case "urgent", "skip-checks", "override-pause":
			if seen[field] {
				return out, false
			}
			seen[field] = true
			switch field {
			case "urgent":
				out.Urgent = true
			case "skip-checks":
				out.SkipChecks = true
			case "override-pause":
				out.OverridePause = true
			}
		default:
			rest = append(rest, field)
		}
	}
	out.Action, out.Count = command(strings.Join(rest, " "), bot)
	if out.Action == "" || out.Action == "cancel" && (out.Urgent || out.SkipChecks || out.OverridePause) {
		return out, false
	}
	if hasReason {
		out.Reason = strings.TrimSpace(reason)
		if out.Reason == "" || len(out.Reason) > 2048 {
			return out, false
		}
	}
	if (out.SkipChecks || out.OverridePause) && out.Reason == "" {
		return out, false
	}
	return out, true
}

type frozenIntent struct {
	Revisions                 []core.Revision
	Reason                    string
	SkipChecks, OverridePause bool
}
type intentState struct {
	Version  int
	Floor    int64
	Requests map[int64]frozenIntent
}

func (g *GitHub) freezeIntent(ctx context.Context, r request, members []core.Candidate) error {
	g.intentMu.Lock()
	defer g.intentMu.Unlock()
	if len(members) > 64 {
		return fmt.Errorf("emergency prefix exceeds 64 changes")
	}
	state := intentState{Version: 1, Requests: map[int64]frozenIntent{}}
	var data []byte
	file, err := os.Open(g.p.IntentPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(file, (2<<20)+1))
		file.Close()
		if err != nil {
			return err
		}
		if len(data) > 2<<20 || json.Unmarshal(data, &state) != nil || state.Version != 1 || state.Requests == nil {
			return fmt.Errorf("invalid GitHub emergency intent state")
		}
	}
	if r.ID <= state.Floor {
		return fmt.Errorf("emergency request expired; post a new request")
	}
	intent := frozenIntent{Reason: r.Reason, SkipChecks: r.SkipChecks, OverridePause: r.OverridePause}
	for _, c := range members {
		intent.Revisions = append(intent.Revisions, core.Revision{Ref: c.Ref, SHA: c.SHA, Version: c.Version})
	}
	if old, exists := state.Requests[r.ID]; exists {
		oldData, _ := json.Marshal(old)
		newData, _ := json.Marshal(intent)
		if string(oldData) != string(newData) {
			if old.Reason != intent.Reason || old.SkipChecks != intent.SkipChecks || old.OverridePause != intent.OverridePause || len(intent.Revisions) >= len(old.Revisions) {
				return fmt.Errorf("emergency revisions or request changed; post a new request")
			}
			present := map[string]bool{}
			index := 0
			for _, rev := range intent.Revisions {
				for index < len(old.Revisions) && old.Revisions[index].Ref != rev.Ref {
					index++
				}
				if index == len(old.Revisions) || old.Revisions[index] != rev {
					return fmt.Errorf("emergency revision changed; post a new request")
				}
				present[rev.Ref] = true
				index++
			}
			branch := ""
			for b, target := range g.p.Targets {
				if target == members[0].Target {
					branch = b
					break
				}
			}
			for _, rev := range old.Revisions {
				if !present[rev.Ref] {
					landed, err := g.p.Git.ReviewLanded(ctx, branch, rev.Ref, rev.SHA)
					if err != nil {
						return err
					}
					if !landed {
						return fmt.Errorf("emergency prefix changed; post a new request")
					}
				}
			}
		}
		return nil
	}
	state.Requests[r.ID] = intent
	for {
		data, _ = json.Marshal(state)
		if len(state.Requests) <= 500 && len(data) <= 1<<20 {
			break
		}
		ids := make([]int64, 0, len(state.Requests))
		for id := range state.Requests {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		state.Floor = ids[0]
		delete(state.Requests, ids[0])
		if r.ID <= state.Floor {
			return fmt.Errorf("emergency request too large or expired; post a new request")
		}
	}
	return statefile.Write(g.p.IntentPath, data)
}
