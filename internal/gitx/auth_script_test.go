package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runAskpass executes the askpass helper the way git would: one prompt
// argument, credentials in the environment.
func runAskpass(t *testing.T, prompt, host string) (string, error) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "askpass.sh")
	if err := os.WriteFile(script, []byte(askpassScript), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script, prompt)
	cmd.Env = append(os.Environ(),
		"GAUNTLET_ASKPASS_HOST="+host,
		"GAUNTLET_ASKPASS_USER=x-access-token",
		"GAUNTLET_ASKPASS_PASS=ghs_FAKESECRET",
	)
	out, err := cmd.Output()
	return string(out), err
}

func TestAskpassScript(t *testing.T) {
	// Username prompt for the expected host.
	out, err := runAskpass(t, "Username for 'https://github.com': ", "github.com")
	if err != nil || out != "x-access-token\n" {
		t.Errorf("username prompt: %q, %v", out, err)
	}
	// Password prompt for the expected host (git includes the username).
	out, err = runAskpass(t, "Password for 'https://x-access-token@github.com': ", "github.com")
	if err != nil || out != "ghs_FAKESECRET\n" {
		t.Errorf("password prompt: %q, %v", out, err)
	}
	// A prompt naming ANY other host — an unexpected redirect, a rogue
	// submodule — gets a refusal and zero bytes, never the token.
	out, err = runAskpass(t, "Password for 'https://evil.example.com': ", "github.com")
	if err == nil {
		t.Error("foreign-host prompt: exit 0, want a refusal")
	}
	if out != "" {
		t.Errorf("foreign-host prompt leaked output %q", out)
	}
}
