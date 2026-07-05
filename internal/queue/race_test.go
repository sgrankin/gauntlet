//go:build race

package queue

// raceScenariosSerial gates TestScriptReal's per-scenario parallelism
// (script_test.go) when the binary is built with -race.
//
// Diagnosis (2026-07-05, see DESIGN.md Watch items): testscript.Run calls
// t.Parallel() unconditionally for every scenario subtest — there is no
// Params knob to opt out. TestScriptReal's scenarios each spin up a real-git
// integrationHarness and spawn several `git` child processes via os/exec
// (internal/testutil/remote.go, internal/gitx), so with every scenario file
// running in parallel plus TestScriptFake's own subtests in the same `go
// test` invocation, dozens of concurrent os/exec.Cmd.Start calls can be
// forking at once.
//
// Under -race that occasionally hangs forever: goroutine dumps captured on
// timeout (go test ./internal/queue/ -race -run TestScriptReal, looped under
// two simultaneous invocations for pressure) show the parent stuck in
// syscall.forkExec -> syscall.readlen, blocked reading the exec-status pipe
// that a just-forked child writes to after a failed exec (or that closes on
// a successful one). The child never gets there: fork() only duplicates the
// calling thread, and forking out of a heavily-threaded, TSan-instrumented
// process can copy in a libSystem/TSan lock held by some other thread at
// that instant, wedging the child before it reaches exec. The parent then
// waits on that pipe forever. Reproduced ~1-in-8 runs under load with
// -race; 0/16 runs without it in the same loop — never observed absent the
// race detector, consistent with this being a TSan-instrumented-fork issue
// rather than a bug in gauntlet's own code.
//
// Mitigation: serialize TestScriptReal's scenarios under -race only, via a
// custom testscript.T (serialScriptT, script_test.go) whose Parallel() is a
// no-op instead of calling testscript.Run directly. This removes the
// concurrent-fork pressure that triggers the deadlock without touching any
// production code (internal/executor, internal/gitx) and without changing
// behavior for the common (non-race) case, where these scenarios run in
// parallel same as before. If CI on Linux ever shows this hang, re-open —
// the working theory is macOS-specific fork/TSan interaction, not a
// portable one.
const raceScenariosSerial = true
