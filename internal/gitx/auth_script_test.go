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
	accept := []struct{ prompt, host string }{
		{"Username for 'https://github.com': ", "github.com"},
		{"Password for 'https://x-access-token@github.com': ", "github.com"},
		// Explicit ports travel in the host.
		{"Password for 'https://x-access-token@ghe.example.com:8443': ", "ghe.example.com:8443"},
		// credential.useHttpPath host config: the prompt carries a path,
		// so the host is "/"-terminated instead of quote-terminated.
		{"Password for 'https://x-access-token@github.com/acme/widgets.git': ", "github.com"},
	}
	for _, tc := range accept {
		out, err := runAskpass(t, tc.prompt, tc.host)
		if err != nil {
			t.Errorf("prompt %q host %q: refused (%v), want an answer", tc.prompt, tc.host, err)
			continue
		}
		want := "ghs_FAKESECRET\n"
		if len(tc.prompt) > 0 && tc.prompt[0] == 'U' {
			want = "x-access-token\n"
		}
		if out != want {
			t.Errorf("prompt %q host %q: output %q, want %q", tc.prompt, tc.host, out, want)
		}
	}

	// A prompt naming ANY other host — an unexpected redirect, a rogue
	// submodule — gets a refusal and zero bytes, never the token. The
	// hostile superstring shapes are the review-confirmed bypass of a
	// naive substring match: every one of these CONTAINS "github.com".
	reject := []string{
		"Password for 'https://evil.example.com': ",
		"Password for 'https://x-access-token@github.com.evil.example': ",
		"Password for 'https://x-access-token@evil-github.com': ",
		"Password for 'https://x-access-token@evilgithub.com': ",
		"Username for 'https://github.com.evil.example': ",
	}
	for _, prompt := range reject {
		out, err := runAskpass(t, prompt, "github.com")
		if err == nil {
			t.Errorf("hostile prompt %q: exit 0, want a refusal", prompt)
		}
		if out != "" {
			t.Errorf("hostile prompt %q leaked output %q", prompt, out)
		}
	}
	// A ported scope must not match its own unported superstring either.
	out, err := runAskpass(t, "Password for 'https://x-access-token@ghe.example.com:84431': ", "ghe.example.com:8443")
	if err == nil || out != "" {
		t.Errorf("port-superstring prompt answered: %q, %v", out, err)
	}
}
