//go:build !race

package deploy_test

// raceScenariosSerial is false in the common (non-race) build: see
// race_test.go for why the race build serializes TestScriptReal's
// scenarios instead of running them in parallel.
const raceScenariosSerial = false
