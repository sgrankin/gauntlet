# Commit signing

## Commit signing

Signing is opt-in. Without a `signing` node, Gauntlet creates unsigned commits,
regardless of the host's `commit.gpgsign` setting. To enable SSH signatures:

```kdl
signing {
    ssh-key "/etc/gauntlet/signing_ed25519.pub"
    timeout "10s"
}
```

The absolute path may name a private key or a public key whose private half is
available through the daemon's `SSH_AUTH_SOCK`. Use OpenSSH 8.2 or newer. For an
agent-backed key, create it with `ssh-keygen -t ed25519 -f signing_ed25519`, add
the private key to the daemon's agent with `ssh-add`, and configure the public
key path. Unlock passphrase-protected keys before starting the daemon; signing
does not launch an askpass dialog. Keep keys outside the bare repository and
candidate work directories. The daemon removes `SSH_AUTH_SOCK` from local
candidate check environments when signing is enabled; local checks still run
as the same OS user, so this is not a filesystem or process isolation boundary.

Gauntlet signs landing commits and receipt-note commits. It preserves the source
author and jj `change-id` header, replaces invalidated source signatures, and
signs the final landing bytes **before verification**. The target receives that
exact tested commit. A signing error stops the operation, with no unsigned
fallback. Signing runs in the reconcile loop with a bounded timeout: the default
is 10 seconds and the maximum is one minute. `gauntlet doctor` checks the signer
executable and key file; creating a commit exercises the actual signing authority.

For GitHub's Verified badge, upload the public key as a **Signing key** on the
signing account and use that account's verified email in the daemon's `committer`
configuration. GitHub App or token authentication authorizes pushes; it does not
sign these locally generated commits. GitHub's account verification and vigilant
mode policies still apply.

For local verification, create an allowed-signers file containing a line such as
`bot@example.com ssh-ed25519 AAAA…` using the configured committer email and public
key, then run:

```sh
git -c gpg.format=ssh -c gpg.ssh.allowedSignersFile=/etc/gauntlet/allowed_signers verify-commit SHA
```
