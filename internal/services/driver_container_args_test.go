package services

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
)

// TestCreateArgs_Resources confirms --memory/--cpus are emitted verbatim
// when set on the spec and omitted entirely when left zero (services.md §7
// "Resource honesty" phase-B landing) — no gauntlet-chosen flag fills in for
// an author who left these unset. Unlike the rest of Create, this is
// unit-testable without a real docker/podman daemon since createArgs
// (driver_container.go) is pure.
func TestCreateArgs_Resources(t *testing.T) {
	base := InstanceSpec{
		Key:  "somekey",
		Name: "gauntlet-svc-tok-somekey",
		Mode: ModePublish,
		Spec: config.Service{Image: "redis:7-alpine", Port: 6379},
	}

	t.Run("unset emits neither flag", func(t *testing.T) {
		args := createArgs(base)
		if containsArg(args, "--memory") || containsArg(args, "--cpus") {
			t.Errorf("createArgs with no Memory/CPUs = %v, want neither --memory nor --cpus", args)
		}
	})

	t.Run("set passes through verbatim", func(t *testing.T) {
		is := base
		is.Spec.Memory = "2g"
		is.Spec.CPUs = "1.5"
		args := createArgs(is)
		if !hasFlagValue(args, "--memory", "2g") {
			t.Errorf("createArgs = %v, want --memory 2g", args)
		}
		if !hasFlagValue(args, "--cpus", "1.5") {
			t.Errorf("createArgs = %v, want --cpus 1.5", args)
		}
	})
}

func containsArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func hasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}
