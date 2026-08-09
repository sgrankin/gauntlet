//go:build race

package deploy_test

// raceScenariosSerial gates TestScriptReal's per-scenario parallelism
// (script_test.go) when the binary is built with -race.
//
// Copied verbatim in effect from internal/queue/race_test.go, whose
// comment carries the full diagnosis: testscript.Run calls t.Parallel()
// for every scenario subtest with no knob to opt out, and this package's
// real harness spawns several `git` child processes per command via
// os/exec (internal/testutil, internal/gitx). Forking out of a
// heavily-threaded, TSan-instrumented process can copy in a lock held by
// another thread and wedge the child before it reaches exec, leaving the
// parent blocked forever on the exec-status pipe. Serializing the
// scenarios under -race removes the concurrent-fork pressure without
// touching production code or changing behavior for the common build.
const raceScenariosSerial = true
