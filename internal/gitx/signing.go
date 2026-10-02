package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type sshSigning struct {
	key     string
	timeout time.Duration
}

// WithSSHSigning signs final commit bytes, including nonstandard jj headers.
// The key and any SSH agent belong to the daemon, not repository commands.
func WithSSHSigning(key string, timeout time.Duration) Option {
	return func(r *Repo) { r.signing = &sshSigning{key: key, timeout: timeout} }
}

func (r *Repo) writeCommit(ctx context.Context, payload string) (string, error) {
	if r.signing != nil {
		cctx, cancel := context.WithTimeout(ctx, r.signing.timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, "ssh-keygen", "-Y", "sign", "-n", "git", "-f", r.signing.key)
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = append(os.Environ(), "SSH_ASKPASS_REQUIRE=never")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = time.Second
		stdout, stderr := &signBuffer{}, &signBuffer{}
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			if cctx.Err() != nil {
				return "", fmt.Errorf("gitx: SSH signing: %w", cctx.Err())
			}
			return "", fmt.Errorf("gitx: SSH signing: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		signature := strings.TrimSpace(stdout.String())
		if stdout.overflow || !strings.HasPrefix(signature, "-----BEGIN SSH SIGNATURE-----\n") || !strings.HasSuffix(signature, "\n-----END SSH SIGNATURE-----") {
			return "", fmt.Errorf("gitx: SSH signer returned an invalid signature")
		}
		headers, message, ok := strings.Cut(payload, "\n\n")
		if !ok {
			return "", fmt.Errorf("gitx: malformed commit payload")
		}
		// Git signs the commit without gpgsig. Continuation lines carry one
		// leading space; removing this header recovers exactly the signed bytes.
		payload = headers + "\ngpgsig " + strings.ReplaceAll(signature, "\n", "\n ") + "\n\n" + message
	}
	out, err := runGit(ctx, r.dir, strings.NewReader(payload), "hash-object", "-t", "commit", "-w", "--stdin")
	return strings.TrimSpace(out), err
}

// Bound diagnostics as well as the signature from a misbehaving subprocess.
type signBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *signBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (64 << 10) - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	b.Buffer.Write(p)
	return n, nil
}
