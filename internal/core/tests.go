package core

// Test IDs are supplied by the repository's test runner and remain stable across
// attempts. Skipped tests never clear a previously failed test.
type TestCase struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type TestAttempt struct {
	Command []string   `json:"command,omitempty"`
	LogPath string     `json:"logPath,omitempty"`
	Number  int        `json:"number"`
	Tests   []TestCase `json:"tests"`
}
