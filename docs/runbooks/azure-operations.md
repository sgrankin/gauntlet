# Azure VM operations

## Optional: expose the dashboard over Tailscale

The NSG above opens SSH only — no inbound rule for 8080, on either the az
CLI or Terraform path. If you want the dashboard/API/MCP reachable from
somewhere other than an SSH tunnel (a laptop, a teammate's box), put it on
your tailnet instead of opening a port: `tailscale serve` proxies a
localhost port onto the tailnet over HTTPS with a MagicDNS name, so the
daemon config never changes — `dashboard "localhost:8080"` stays exactly
as every other phase in this doc has it, no rebind to `0.0.0.0`.

1. Install and join, using a **pre-authorized, tagged** auth key (generate
   one in the Tailscale admin console, scoped to e.g. `tag:ci`) rather than
   an interactive login — a tagged key lets this unattended builder join
   headless, and tagged nodes don't expire the way a personal-account
   node's key would out from under it:

   ```sh
   curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg \
     | sudo tee /usr/share/keyrings/tailscale-archive-keyring.gpg >/dev/null
   curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list \
     | sudo tee /etc/apt/sources.list.d/tailscale.list
   sudo apt-get update && sudo apt-get install -y tailscale
   sudo tailscale up --auth-key=<TAILSCALE_AUTH_KEY>
   ```

2. Proxy the dashboard's existing localhost port onto the tailnet:

   ```sh
   sudo tailscale serve --bg 8080
   ```

3. **VERIFY** — find the tailnet URL and confirm it answers:

   ```sh
   tailscale serve status
   # expect: an https://<VM_NAME>.<TAILNET>.ts.net URL mapped to 127.0.0.1:8080
   curl -s https://<VM_NAME>.<TAILNET>.ts.net/api/v1/status | jq '.targets | length'
   # expect: a number > 0, over HTTPS, from any device on the tailnet — no SSH tunnel needed
   ```

**Trust note:** anyone on the tailnet can now reach the dashboard **and**
its mutating endpoints (`retry`/`cancel`) and `/mcp` — the dashboard/API/MCP
still have no authentication of their own (same trust model as
[deploy.md's exposure guidance](../operations/security.md#dashboard-api-and-mcp));
Tailscale is the access boundary here, not an additional auth layer on top
of gauntlet itself. Scope tailnet ACLs accordingly if not everyone on it
should reach this.

This composes with Phase 4's park/wake automation with no extra work:
`tailscaled` reconnects automatically on `tailscale up`'s next boot after a
wake, and the `tailscale serve` config persists across a deallocate/start
cycle (it's stored on the VM's own disk, not re-run at every boot) — no
part of this section needs to be redone after a park/wake, only after the
recovery drill's from-scratch VM recreate.

## Phase 4 — Park/wake automation (cost control)

A deallocated VM keeps its disks (so every warm docker layer, module cache,
and `history.db` survives) and bills storage only, not compute — the
mechanism [design/scaling.md](../architecture/scaling.md)'s "Axis 2" sketches. Two timer-driven pieces, either
as an Azure Function or a cron job on some other always-on box:

**Wake** — a parked daemon can't see refs arrive, so wake-on-work has to
poll the remote from outside:

```sh
if [ -n "$(git ls-remote <REMOTE_URL> 'refs/heads/for/*')" ]; then
  az vm start --resource-group <RESOURCE_GROUP> --name <VM_NAME>
fi
```

**Park** — poll the daemon's own idle signal and deallocate once it's been
idle past your threshold:

```sh
IDLE_SINCE=$(curl -fsS http://<VM_IP>:8080/api/v1/status | jq -r '.idleSince // empty')
if [ -n "$IDLE_SINCE" ]; then
  IDLE_SECS=$(( $(date +%s) - $(date -d "$IDLE_SINCE" +%s) ))
  if [ "$IDLE_SECS" -gt $((30 * 60)) ]; then    # N = 30 min, tune to taste
    az vm deallocate --resource-group <RESOURCE_GROUP> --name <VM_NAME>
  fi
fi
```

`idleSince` (an RFC3339 timestamp, absent/empty when the daemon isn't idle)
is true daemon-wide idleness — every target's queue empty AND no post-land
hook running or backlogged — not just "no candidate right now for one
target." Gate on it rather than on queue depth alone, or a park can race a
hook that's still running.

**Minimal Azure Function sketch** (Python, timer trigger, combining both
checks in one run):

```python
import subprocess, json, urllib.request, datetime, os

def main(mytimer):
    remote = os.environ["GAUNTLET_REMOTE"]
    vm = os.environ["GAUNTLET_VM_NAME"]
    rg = os.environ["GAUNTLET_RESOURCE_GROUP"]
    dashboard = os.environ["GAUNTLET_DASHBOARD_URL"]

    has_work = bool(subprocess.run(
        ["git", "ls-remote", remote, "refs/heads/for/*"],
        capture_output=True, text=True).stdout.strip())

    if has_work:
        subprocess.run(["az", "vm", "start", "-g", rg, "-n", vm], check=True)
        return

    try:
        status = json.load(urllib.request.urlopen(f"{dashboard}/api/v1/status", timeout=5))
    except Exception:
        return  # VM likely already deallocated — nothing to do
    idle_since = status.get("idleSince")
    if idle_since:
        idle_for = datetime.datetime.now(datetime.timezone.utc) - datetime.datetime.fromisoformat(idle_since)
        if idle_for > datetime.timedelta(minutes=30):
            subprocess.run(["az", "vm", "deallocate", "-g", rg, "-n", vm], check=True)
```

**Deallocate-mid-run is crash-equivalent, not data loss.** If the timer
races a still-in-flight run, deallocation just interrupts it the same way a
host crash would — gauntlet's recovery already handles this unconditionally
(no durable in-flight state; a restart rescans refs from scratch, see
deploy-linux.md's upgrade-procedure rationale). A hook mid-run may be
skipped on recovery rather than resumed. The `idleSince` gate exists purely
to make this the *rare* case instead of the *routine* one — an unlucky race
is safe, not just tolerated.

**VERIFY**

```sh
curl -s http://<VM_IP>:8080/api/v1/status | jq '.idleSince'
az vm get-instance-view -g <RESOURCE_GROUP> -n <VM_NAME> --query instanceView.statuses[1].displayStatus -o tsv
```

## Phase 5 — Upgrade procedure

```sh
URL=$(curl -fsSL https://api.github.com/repos/sgrankin/gauntlet/releases/latest \
  | grep -o '"browser_download_url": *"[^"]*gauntlet_linux_amd64"' \
  | cut -d'"' -f4)
ssh <ADMIN_USER>@<VM_IP> "sudo curl -fsSL -o /usr/local/bin/gauntlet '$URL' && sudo chmod +x /usr/local/bin/gauntlet && sudo systemctl restart gauntlet"
```

Safe at any time, same "no durable in-flight state" argument as
deploy-linux.md's upgrade procedure — a restart mid-trial just retries a few
seconds later. OS packages patch themselves via `unattended-upgrades`
(enabled in Phase 3); this step only ever touches the `gauntlet` binary.

**VERIFY** — run the full [verify.md](verify.md) checklist after every
upgrade, not just a version-string check; `journalctl -u gauntlet -n 30` for
a boot-quiet sanity check first.

## Health signal

No separate monitoring stack for a single pet VM — the merge queue's own
surfaces are the health signal: `systemd`'s `Restart=on-failure` (the unit
in deploy-linux.md) recovers a crashed process automatically, the
flock-per-state-dir turns "two daemons somehow running" into a refused
startup instead of silent corruption, and a genuinely stuck queue is
directly visible on `GET /api/v1/status` (a target's `current` run not
advancing across repeated polls) — wire that single endpoint into whatever
alerting the operator already runs elsewhere, rather than standing up a new
stack just for this VM.

## Recovery drill

Actually rehearse this — a playbook that's never been exercised is a guess,
not a runbook. Do this against a non-production VM first if you have any
doubt.

1. **Deallocate:**

   ```sh
   az vm deallocate --resource-group <RESOURCE_GROUP> --name <VM_NAME>
   ```

2. **Delete the VM, keeping its disks** (the whole point of the disk split
   — a data disk attached via `az vm disk attach`, as in Phase 2, defaults
   to "detach" rather than "delete" on VM deletion, unlike the OS disk,
   which defaults to deleting with the VM — that asymmetry is exactly what
   makes the disposable-OS-disk/durable-data-disk split work with no extra
   flags):

   ```sh
   az vm delete --resource-group <RESOURCE_GROUP> --name <VM_NAME> --yes
   ```

   **VERIFY** the data disk survived the VM delete:

   ```sh
   az disk show --resource-group <RESOURCE_GROUP> --name <VM_NAME>-state --query diskState -o tsv
   # expect: Unattached (not an error, not "not found")
   ```

3. **Recreate the VM from the stock image** (Phase 1 again, same
   `first-boot.sh`, new or reused VM name):

   ```sh
   az vm create \
     --resource-group <RESOURCE_GROUP> \
     --name <VM_NAME> \
     --image Ubuntu2404 \
     --size Standard_D8s_v5 \
     --zone 1 \
     --admin-username <ADMIN_USER> \
     --ssh-key-values ~/.ssh/id_ed25519.pub \
     --custom-data first-boot.sh \
     --public-ip-sku Standard
   ```

   (`--zone` must match the data disk's zone — a Premium v2 disk can only
   attach to a VM in its own zone.)

4. **Reattach the data disk** (Phase 2's `az vm disk attach`, skipping the
   `mkfs.ext4` step — the filesystem and its data already exist, only the
   fstab entry needs re-adding if it's a genuinely fresh OS disk):

   ```sh
   az vm disk attach --resource-group <RESOURCE_GROUP> --vm-name <VM_NAME> --name <VM_NAME>-state
   ssh <ADMIN_USER>@<VM_IP> 'echo "UUID=<SAME_UUID_AS_BEFORE>  /mnt/gauntlet-state  ext4  defaults,nofail  0  2" | sudo tee -a /etc/fstab && sudo mount -a'
   ```

5. **VERIFY end to end** — this is the actual proof the drill worked, not
   just that commands returned zero:

   ```sh
   ssh <ADMIN_USER>@<VM_IP> 'ls /mnt/gauntlet-state/state/history.db && systemctl is-active gauntlet'
   # expect: history.db present (pre-dates this recreate — proves the disk
   # round-tripped), and gauntlet active (systemd started it via
   # first-boot's custom-data / unit install)
   ssh <ADMIN_USER>@<VM_IP> "docker info --format '{{.DockerRootDir}}' && docker images"
   # expect: /mnt/gauntlet-state/docker, and your check/service images
   # already listed with no pull needed — confirms the docker data-root
   # relocation (Phase 3) survived the recreate along with everything else
   # on the data disk
   ```

   Then run [verify.md](verify.md)'s full checklist — a candidate landing
   end-to-end after a from-scratch VM recreate is the real pass/fail signal
   for this whole drill, not any individual `az` command's exit code. A
   check run's build step finishing in warm-cache time rather than a cold
   module download is the practical confirmation that the named `cache`
   volumes rode along with the images, not just the images themselves.

## Backup notes

Only `history.db` deserves separate backup thought — everything else under
`-state` (bare clones, `trials/`, `logs/`) is derivable, per
[deploy.md's "Backup notes"](../operations/storage.md#backups): re-clonable from
the remote, swept on restart, or aged out by `log-retention`. The data-disk
split above already gets you most of the way (a VM delete can't touch it),
but a VM-delete-and-forgot-to-check moment is still possible — a periodic
snapshot is cheap belt-and-braces on top:

```sh
az snapshot create \
  --resource-group <RESOURCE_GROUP> \
  --name <VM_NAME>-state-$(date +%Y%m%d) \
  --source <VM_NAME>-state
```

Run this on a timer (daily, or whatever `history.db`'s value to you
warrants) and prune old snapshots yourself — `az snapshot list`/`delete` has
no built-in retention policy.
