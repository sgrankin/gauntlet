# Immutable Azure VM setup

Terraform replaces the VM on upgrades while retaining its separate data disk.
Use stock Ubuntu with cloud-init; application state and credentials live on the
persistent disk. For a host maintained over SSH, use [Azure VM setup](azure-vm.md).

## Prerequisites

- Azure subscription, resource group, region/zone, and an SSH public key.
- Terraform 1.5+ and Azure credentials (`az login` or a service principal).
- Repository/runtime credentials and a Gauntlet release version.

Create the files in [Terraform and cloud-init templates](azure-immutable-templates.md) before provisioning.

## Phase 3 — First provision + one-time secrets delivery

```sh
terraform apply -var="resource_group=<RESOURCE_GROUP>" -var="admin_cidr=<YOUR_ADMIN_CIDR>" -var="gauntlet_version=<VERSION>"
```

The VM comes up with gauntlet installed, enabled, and **not running** —
by design (cloud-init's comment above). Deliver the config and secrets
once, onto the persistent disk:

```sh
scp gauntlet.kdl gauntlet.env <ADMIN_USER>@<VM_IP>:/tmp/
ssh <ADMIN_USER>@<VM_IP> '
  sudo mv /tmp/gauntlet.kdl /tmp/gauntlet.env /mnt/gauntlet-state/
  sudo chown gauntlet:gauntlet /mnt/gauntlet-state/gauntlet.kdl /mnt/gauntlet-state/gauntlet.env
  sudo chmod 0600 /mnt/gauntlet-state/gauntlet.env
  sudo systemctl start gauntlet
'
```

**Enterprise variant, one paragraph, not built here:** instead of `scp`, an
Azure Key Vault reference + the VM's system-assigned managed identity
(`azurerm_key_vault_secret` + `azurerm_role_assignment` granting the VM's
identity `Key Vault Secrets User`, with a `runcmd` step calling `az keyvault
secret show` at boot to materialize `gauntlet.env`) removes the manual
`scp` step entirely and gets the secret out of anyone's shell history —
worth it once you have Key Vault infrastructure already; overkill to stand
up for a single VM's one `.env` file.

**Because these two files live on the persistent data disk, not the OS
disk, this whole Phase 3 runs exactly once, ever** — every future replace
(Phase 5's upgrade, or the recovery drill) reattaches the same disk with
the same files already on it and starts clean, no re-delivery needed.

**VERIFY** — the full [verify.md](verify.md) checklist, plus:

```sh
ssh <ADMIN_USER>@<VM_IP> systemctl is-active gauntlet
```

## Phase 4 — Tailscale (optional, folded into cloud-init)

Unlike azure-vm.md's manual-once path, here it goes into the same
`runcmd` — but the OS disk (where tailscale's own node state under
`/var/lib/tailscale` normally lives) doesn't survive a replace, so
`tailscale up` re-runs, and the node technically re-joins fresh, on
**every** replace, not just the first boot. That's fine functionally with
a reusable tagged auth key (no manual reauth needed), but pin `--hostname`
explicitly so the MagicDNS name stays stable across replaces even though
the underlying node identity churns:

```yaml
  - curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg | tee /usr/share/keyrings/tailscale-archive-keyring.gpg >/dev/null
  - curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list | tee /etc/apt/sources.list.d/tailscale.list
  - apt-get update && apt-get install -y tailscale
  - tailscale up --auth-key="${tailscale_auth_key}" --hostname="${vm_name}"
  - tailscale serve --bg 8080
```

Pass `tailscale_auth_key` into the same `templatefile()` call as
`gauntlet_download_url` — mark the Terraform variable `sensitive = true`
so it doesn't print in plan/apply output (it still ends up inside
`custom_data`, i.e. inside `terraform.tfstate`, same caveat any Terraform
secret variable carries — this is the one thing in this design that isn't
covered by "secrets live on the data disk, not in TF" above; if that
matters to you, deliver the Tailscale key the same out-of-band way as
`gauntlet.env` in Phase 3 instead, and read it from
`/mnt/gauntlet-state/tailscale.key` in `runcmd`).

## Phase 5 — Upgrade procedure

```sh
terraform apply -var="resource_group=<RESOURCE_GROUP>" -var="admin_cidr=<YOUR_ADMIN_CIDR>" -var="gauntlet_version=<NEW_VERSION>"
```

That's the entire upgrade. Bumping `gauntlet_version` changes the rendered
`cloud-init.yaml`, which changes `custom_data`, which — because
`custom_data` is `ForceNew` on this resource — makes Terraform **destroy
the VM and create a new one** with the new version baked into its first
boot. This is destroy-then-create, Terraform's default for a ForceNew
attribute change, and that default is the *correct* choice here, not
merely the path of least resistance:
`create_before_destroy = true` would try to bring up the replacement VM
*before* tearing down the old one — but the data disk attaches to exactly
one VM at a time (`azurerm_virtual_machine_data_disk_attachment` can't
attach the same managed disk to two VMs concurrently) and the flock on
`-state` refuses a second daemon anyway even if it somehow could. Both
mechanisms would just turn `create_before_destroy` into a guaranteed
attach-or-flock failure on the new VM, for a "no downtime" benefit that
was never actually achievable here. Leave the default alone.

**Downtime is one VM boot** (provision + cloud-init's runcmd — a couple of
minutes, most of it `apt-get install`). Mid-run replacement is
crash-equivalent, recovered the same way any deallocate/restart is
(deploy-linux.md's upgrade-procedure rationale) — but the polite version
checks `idleSince` first rather than relying on that recovery path every
time:

```sh
#!/bin/sh
# before handing off to `terraform apply`.
set -eu
DASHBOARD_URL="$1"; TIMEOUT_SECS="${2:-1800}"
elapsed=0
while [ "$elapsed" -lt "$TIMEOUT_SECS" ]; do
  idle=$(curl -fsS "$DASHBOARD_URL/api/v1/status" | jq -r '.idleSince // empty')
  [ -n "$idle" ] && exit 0
  sleep 30
  elapsed=$((elapsed + 30))
done
echo "wait-for-idle: timed out after ${TIMEOUT_SECS}s, queue still busy" >&2
exit 1
```

```sh
./wait-for-idle.sh http://<VM_IP>:8080 && terraform apply -var="gauntlet_version=<NEW_VERSION>" ...
```

**VERIFY** — full [verify.md](verify.md) checklist against the new VM
after every upgrade.

## Recovery drill (this IS the upgrade path)

Unlike azure-vm.md's multi-step manual drill, here the drill and the
upgrade mechanism are the same command — that's the point of this whole
design:

```sh
terraform apply -replace="azurerm_linux_virtual_machine.gauntlet"
```

(`-replace` is the current, non-deprecated way to force a resource
replacement; `terraform taint` + `apply` is the older equivalent if you're
on an older Terraform version.) This destroys and recreates the VM with
the *same* `gauntlet_version`, reattaches the *same* data disk, and proves
the whole recreate-from-scratch path works — exactly what azure-vm.md's
manual drill rehearses by hand, done here by construction every time you
upgrade, not just when you remember to drill it.

**Rehearse this before the daemon holds anything you care about.** First
provision against an *empty* data disk, run `-replace` once immediately
(before Phase 3's secrets even exist), and confirm the VM comes back up
enabled-but-not-running exactly as Phase 2 describes — that's the cheapest
possible point to discover a `fs_setup` mistake, long before there's a
`history.db` on that disk worth losing.

**VERIFY**

```sh
ssh <ADMIN_USER>@<VM_IP> 'ls /mnt/gauntlet-state/gauntlet.kdl && systemctl is-active gauntlet'
# expect: gauntlet.kdl present (pre-dates the replace — proves the disk and
# its delivered secrets round-tripped), and gauntlet active
```

Once there's real history to lose, this drill doubles as confirmation the
docker data-root move (Phase 2) is actually holding — a from-scratch VM
should NOT come back with a cold docker:

```sh
ssh <ADMIN_USER>@<VM_IP> docker images
```

Trigger a check run against this fresh VM and compare its build-step timing
to a typical warm run (deploy-linux.md's second-run expectation for the
container executor's caches) — a go-build step finishing in warm-cache time
rather than a full cold-module-download confirms the named `cache` volumes
(not just the images) rode along too.

Then run [verify.md](verify.md) end to end, same as azure-vm.md's drill.

## Backup notes

Identical reasoning to azure-vm.md's: only `history.db` deserves separate
backup thought (everything else under `-state` is derivable, per
[deploy.md's "Backup notes"](../operations/storage.md#backups)), and
`prevent_destroy` on `azurerm_managed_disk.gauntlet_state` already
protects it from the one operation (`terraform destroy`/an errant replace
of the *disk* resource specifically) this design would otherwise risk. A
periodic `az snapshot create` against the same disk (azure-vm.md's Backup
notes section, verbatim) is still cheap belt-and-braces on top — Terraform
doesn't need to own that; run it as a timer independent of this config.

---

**Verification honesty:** every HCL/cloud-init construct above is written
against stable, documented `azurerm` provider and cloud-init module
schemas — `custom_data`'s ForceNew behavior and `fs_setup`'s
`partition: any` semantics were specifically checked against the
provider's/cloud-init's own source and documentation rather than assumed,
since getting either wrong either breaks every upgrade or destroys the
state disk on replace. Nothing in this doc was run against a real Azure
subscription (no `terraform apply` executed) — the one step worth
deliberately de-risking before trusting this against real data is exactly
the recovery-drill-against-an-empty-disk rehearsal called out above.
