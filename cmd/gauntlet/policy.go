package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/policy"
	"io"
	"os"
	"path/filepath"
)

func buildPolicy(cfg *config.Daemon, configPath string) (*policy.Engine, error) {
	if cfg.Policy == nil {
		return nil, nil
	}
	source := cfg.Policy.Rego
	if cfg.Policy.File != "" {
		path := cfg.Policy.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(configPath), path)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
		if err != nil {
			return nil, err
		}
		source = string(data)
	}
	return policy.Compile(context.Background(), source, cfg.Policy.Timeout)
}
func runPolicyCheck(args []string) error {
	fs := flag.NewFlagSet("policy-check", flag.ContinueOnError)
	configPath := fs.String("config", "gauntlet.kdl", "operator configuration")
	inputPath := fs.String("input", "", "versioned policy facts JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadDaemon(*configPath)
	if err != nil {
		return err
	}
	engine, err := buildPolicy(cfg, *configPath)
	if err != nil {
		return err
	}
	if engine == nil {
		return fmt.Errorf("policy not configured")
	}
	data, err := os.ReadFile(*inputPath)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("input too large")
	}
	var input any
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	decision, err := engine.Evaluate(context.Background(), input)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(map[string]any{"policy_version": engine.Version, "decision": decision}, "", "  ")
	fmt.Println(string(out))
	if !decision.Allow {
		return fmt.Errorf("%s", decision.Reason())
	}
	return nil
}
