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
	override := f.Bool("override-pause", false, "allow this emergency request during incident pause")
	if err := f.Parse(args); err != nil {
		return err
	}
	if len(f.Args()) != 1 || *target == "" || strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("control requires -target, -reason, and pause/resume/urgent/merge-anyway")
	}
	cmd := core.Command{Kind: f.Args()[0], Target: *target, Actor: *actor, Reason: *reason, OverridePause: *override}
	if *revisions != "" {
		if err := json.Unmarshal([]byte(*revisions), &cmd.Revisions); err != nil {
			return err
		}
	}
	data, _ := json.Marshal(cmd)
	client := http.Client{Timeout: 20 * time.Second}
	res, err := client.Post(strings.TrimRight(*base, "/")+"/api/v1/control", "application/json", bytes.NewReader(data))
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
