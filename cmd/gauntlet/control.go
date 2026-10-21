package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

func runControl(args []string) error {
	f := flag.NewFlagSet("control", flag.ContinueOnError)
	base := f.String("url", "http://localhost:8080", "daemon admin URL")
	target := f.String("target", "", "target; * for pause/resume all")
	actor := f.String("actor", "", "operator identity for audit")
	reason := f.String("reason", "", "required audit reason")
	revisions := f.String("revisions", "", "JSON array of exact ref/sha/version objects")
	priority := f.String("priority", "normal", "low, normal, high, or urgent")
	before := f.String("before", "", "move before this candidate ref")
	after := f.String("after", "", "move after this candidate ref")
	preempt := f.Bool("preempt", false, "explicitly permit cancelling affected active verification")
	token := f.String("plan-token", "", "apply only if the preview token still matches")
	preview := f.Bool("preview", false, "preview positions and affected verification without changing the queue")
	override := f.Bool("override-pause", false, "allow this emergency request during incident pause")
	if err := f.Parse(args); err != nil {
		return err
	}
	if len(f.Args()) != 1 || *target == "" || strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("control requires -target, -reason, and pause/resume/priority/move/urgent/merge-anyway")
	}
	cmd := core.Command{Kind: f.Args()[0], Target: *target, Actor: *actor, Reason: *reason, OverridePause: *override, Priority: *priority, Before: *before, After: *after, Preempt: *preempt, PlanToken: *token}
	if *revisions != "" {
		if err := json.Unmarshal([]byte(*revisions), &cmd.Revisions); err != nil {
			return err
		}
	}
	data, _ := json.Marshal(cmd)
	client := http.Client{Timeout: 20 * time.Second}
	endpoint := "/api/v1/control"
	if *preview {
		endpoint += "/preview"
	}
	res, err := client.Post(strings.TrimRight(*base, "/")+endpoint, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 65536))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", res.Status, body)
	}
	fmt.Fprintln(os.Stdout, string(body))
	return nil
}
