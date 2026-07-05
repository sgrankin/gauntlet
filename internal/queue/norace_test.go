//go:build !race

package queue

// raceScenariosSerial is false in the common (non-race) build: see
// race_test.go for the full diagnosis of why the race build serializes
// TestScriptReal's scenarios instead of running them in parallel.
const raceScenariosSerial = false
