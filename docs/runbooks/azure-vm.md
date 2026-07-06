# Runbook: gauntlet on an Azure VM (stateful builder)

**What you get:** a single "pet" Ubuntu VM running the `gauntlet` binary
directly (deploy-linux.md's Topology (a), on Azure) — no golden image, no
Packer, no Compute Gallery; state lives on a separate data disk so the VM
itself is disposable and recreatable in minutes. This doc covers the Azure
layer only — provisioning, disks, park/wake, upgrade, and recovery. For the
daemon config, systemd unit, GitHub/Slack setup, and first-run verification,
see [deploy-linux.md](deploy-linux.md) — this runbook references its phases
rather than repeating them.

**Maintainability-averse?** This runbook is a hand-provisioned pet VM —
`az` CLI commands (or the Terraform variant below) get it running, but
day-2 upgrades still involve SSH and a script. If you'd rather never SSH in
to fix drift and have every upgrade be a `terraform apply`, see
[azure-vm-immutable.md](azure-vm-immutable.md) instead — same data-disk/
recovery philosophy, but the VM itself is replaced, never patched.

**Prerequisites**

- `az` CLI logged in (`az login`) with a subscription selected
  (`az account set --subscription <SUBSCRIPTION_ID>`).
- A resource group (`az group create -n <RESOURCE_GROUP> -l <REGION>`).
- An SSH key pair (`~/.ssh/id_ed25519[.pub]` or equivalent) for VM access.
- Everything deploy-linux.md's own prerequisites list (git ≥2.38 ships with
  Ubuntu LTS already; docker is installed by the first-boot script below,
  not assumed present).

One clarifying note since the question comes up: this VM boots from an
Azure **managed-disk image** (a stock Ubuntu marketplace image), not a
docker image — the docker *images* your checks use are a separate,
unrelated layer that docker pulls after the VM is already running.

---

## Phase 1 — Provision the VM

```sh
az vm create \
  --resource-group <RESOURCE_GROUP> \
  --name <VM_NAME> \
  --image Ubuntu2404 \
  --size Standard_D4s_v5 \
  --admin-username <ADMIN_USER> \
  --ssh-key-values ~/.ssh/id_ed25519.pub \
  --custom-data first-boot.sh \
  --public-ip-sku Standard
```

- **Sizing:** `Standard_D4s_v5` (4 vCPU / 16 GiB RAM) is the floor for a
  builder running anything SQL-Server-class in the container executor's
  `services` block (README's ["Shared services"](../../README.md#shared-services)
  wants headroom beyond the container's own `memory`/`cpus` ceiling for the
  host + docker daemon + other checks running concurrently). A plain Go/lint
  builder with no heavy services can go smaller.
- **No spot instance, on purpose** — spot eviction is exactly the kind of
  infra error `auto-retry-errors` (README's ["Retry
  semantics"](../../README.md#landing-changes), already shipped) exists to
  absorb once, but stacking spot eviction *and* the park/wake deallocation
  below multiplies infra-error surface for no real savings at this scale
  (docs/plans/scale.md §5's own framing: revisit spot only once a
  remote-executor/multi-worker story exists, §3 of that doc — not before).
  A regular VM parked overnight beats spot complexity today.
- `first-boot.sh` (referenced by `--custom-data`) is written in Phase 3
  below — for this first `az vm create`, either write it first or create
  the VM without `--custom-data` and run the script by hand once over SSH.
- `Ubuntu2404` is az CLI's image alias for the current Ubuntu LTS
  marketplace image; if it 404s for your subscription/region, list current
  aliases with `az vm image list --all -p Canonical -o table` and
  substitute the exact URN.

**VERIFY**

```sh
az vm show -g <RESOURCE_GROUP> -n <VM_NAME> --query "provisioningState" -o tsv
# expect: Succeeded
ssh <ADMIN_USER>@$(az vm show -g <RESOURCE_GROUP> -n <VM_NAME> -d --query publicIps -o tsv) echo ok
# expect: ok
```

## Phase 2 — Attach the data disk

The OS disk is disposable (recreate from the stock image anytime). Everything
that needs to survive a VM recreate — the daemon's `-state` dir, most
importantly `history.db` — lives on a **separate** managed disk instead.

```sh
az disk create \
  --resource-group <RESOURCE_GROUP> \
  --name <VM_NAME>-state \
  --size-gb 64 \
  --sku Premium_LRS

az vm disk attach \
  --resource-group <RESOURCE_GROUP> \
  --vm-name <VM_NAME> \
  --name <VM_NAME>-state
```

On the VM, format (first attach only — skip this on a re-attach to an
existing disk, or you'll destroy its data) and mount by UUID, not device
path (device names like `/dev/sdc` aren't guaranteed stable across reboots
or re-attach):

```sh
# find the unformatted disk (first attach only)
lsblk
sudo mkfs.ext4 /dev/sdc
sudo mkdir -p /mnt/gauntlet-state
UUID=$(sudo blkid -s UUID -o value /dev/sdc)
echo "UUID=$UUID  /mnt/gauntlet-state  ext4  defaults,nofail  0  2" | sudo tee -a /etc/fstab
sudo mount -a
```

`nofail` matters here: without it, a boot where the data disk hasn't
finished attaching yet (or is briefly detached mid-recovery-drill, below)
drops you to an emergency shell instead of just booting without the mount.

**VERIFY**

```sh
mount | grep gauntlet-state
# expect: /dev/sdc on /mnt/gauntlet-state type ext4
df -h /mnt/gauntlet-state
# expect: a ~64G filesystem, not the OS disk's size
```

## Phase 3 — First-boot script

`first-boot.sh` (passed as `--custom-data` in Phase 1, or run once by hand)
does everything deploy-linux.md's Phases 1–5 do, adapted for Azure's disk
layout — install docker, fetch the `gauntlet` binary from the latest
GitHub release, lay out `/etc/gauntlet`, and install the systemd unit
pointing `-state` at the mounted data disk:

```sh
#!/bin/sh
set -eu

# Docker engine from docker's own apt repo (not Ubuntu's, which lags).
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /usr/share/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io git unattended-upgrades

# OS patching on autopilot — no separate patch-management tooling for a
# single pet VM.
sudo dpkg-reconfigure -f noninteractive unattended-upgrades

# Fetch the gauntlet binary from the latest tagged GitHub release
# (goreleaser-built linux_amd64 tarball — see .goreleaser.yaml). Match by
# pattern rather than a hardcoded filename so this survives version bumps
# and any archive-naming tweak without edits here.
URL=$(curl -fsSL https://api.github.com/repos/sgrankin/gauntlet/releases/latest \
  | grep -o '"browser_download_url": *"[^"]*linux_amd64\.tar\.gz"' \
  | cut -d'"' -f4)
curl -fsSL "$URL" | sudo tar -xz -C /usr/local/bin gauntlet

sudo mkdir -p /etc/gauntlet
sudo mkdir -p /mnt/gauntlet-state/state
# gauntlet.kdl and gauntlet.env are NOT written here — provision them
# out-of-band (they carry your remote URL and secrets); see deploy-linux.md
# Phases 2–3 for their content, same on Azure as anywhere else.
```

Then install the systemd unit exactly as deploy-linux.md's Phase 5, with
one change — `ExecStart`'s `-state` points at the mounted data disk:

```ini
ExecStart=/usr/local/bin/gauntlet -config /etc/gauntlet/gauntlet.kdl -state /mnt/gauntlet-state/state
```

**VERIFY** — run deploy-linux.md's Phase 5 VERIFY (`systemctl is-active
gauntlet`, `journalctl -u gauntlet -n 30`), then the full
[verify.md](verify.md) checklist once, end to end.

## Terraform variant (provisioning phases 1–3)

If you'd rather provision declaratively than run Phases 1–3's `az` commands
by hand, here's the same shape as Terraform (`azurerm` provider) — resource
group, minimal networking (vnet/subnet/NIC, NSG allowing SSH only — the
dashboard stays localhost-bound per
[deploy.md's exposure guidance](../deploy.md#dashboard--api--mcp-exposure-guidance),
so no public NSG rule for port 8080), the VM, and the data disk +
attachment. Not a full reusable module — copy, rename, and fill in
placeholders:

```hcl
terraform {
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}

provider "azurerm" {
  features {}
}

resource "azurerm_resource_group" "gauntlet" {
  name     = "<RESOURCE_GROUP>"
  location = "<REGION>"
}

# --- networking ---

resource "azurerm_virtual_network" "gauntlet" {
  name                = "<VM_NAME>-vnet"
  address_space       = ["10.0.0.0/16"]
  location            = azurerm_resource_group.gauntlet.location
  resource_group_name = azurerm_resource_group.gauntlet.name
}

resource "azurerm_subnet" "gauntlet" {
  name                 = "<VM_NAME>-subnet"
  resource_group_name  = azurerm_resource_group.gauntlet.name
  virtual_network_name = azurerm_virtual_network.gauntlet.name
  address_prefixes     = ["10.0.1.0/24"]
}

resource "azurerm_network_security_group" "gauntlet" {
  name                = "<VM_NAME>-nsg"
  location            = azurerm_resource_group.gauntlet.location
  resource_group_name = azurerm_resource_group.gauntlet.name

  security_rule {
    name                       = "AllowSSH"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefix      = "<YOUR_ADMIN_CIDR>"   # narrow this — never 0.0.0.0/0
    destination_address_prefix = "*"
  }
  # No rule for 8080 (or any other port) here, deliberately — reach the
  # dashboard over an SSH tunnel or tailnet, never a public inbound rule.
  # See "Optional: expose the dashboard over Tailscale" below for the
  # tailnet path.
}

resource "azurerm_public_ip" "gauntlet" {
  name                = "<VM_NAME>-ip"
  location            = azurerm_resource_group.gauntlet.location
  resource_group_name = azurerm_resource_group.gauntlet.name
  allocation_method   = "Static"
  sku                 = "Standard"
}

resource "azurerm_network_interface" "gauntlet" {
  name                = "<VM_NAME>-nic"
  location            = azurerm_resource_group.gauntlet.location
  resource_group_name = azurerm_resource_group.gauntlet.name

  ip_configuration {
    name                          = "internal"
    subnet_id                     = azurerm_subnet.gauntlet.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.gauntlet.id
  }
}

resource "azurerm_network_interface_security_group_association" "gauntlet" {
  network_interface_id      = azurerm_network_interface.gauntlet.id
  network_security_group_id = azurerm_network_security_group.gauntlet.id
}

# --- the VM (Phase 1, as HCL) ---

resource "azurerm_linux_virtual_machine" "gauntlet" {
  name                   = "<VM_NAME>"
  resource_group_name    = azurerm_resource_group.gauntlet.name
  location               = azurerm_resource_group.gauntlet.location
  size                   = "Standard_D4s_v5"   # same sizing rationale as Phase 1
  admin_username         = "<ADMIN_USER>"
  network_interface_ids  = [azurerm_network_interface.gauntlet.id]

  admin_ssh_key {
    username   = "<ADMIN_USER>"
    public_key = file("~/.ssh/id_ed25519.pub")
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "Standard_LRS"   # OS disk stays disposable/cheap — the data disk below is the durable one
  }

  # Stock Ubuntu LTS marketplace image — Terraform has no alias shorthand
  # for the az CLI's `--image Ubuntu2404`, so publisher/offer/sku are pinned
  # explicitly instead.
  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }
  # If this combination 404s for your region/subscription (marketplace
  # offer/sku names shift over time), list current values with:
  # az vm image list-skus -l <REGION> -p Canonical -f ubuntu-24_04-lts -o table

  # Same first-boot.sh as Phase 3, base64'd — custom_data has identical
  # cloud-init/first-boot-script semantics to az vm create's --custom-data.
  custom_data = base64encode(file("${path.module}/first-boot.sh"))
}

# --- data disk (Phase 2, as HCL) ---

resource "azurerm_managed_disk" "gauntlet_state" {
  name                 = "<VM_NAME>-state"
  location             = azurerm_resource_group.gauntlet.location
  resource_group_name  = azurerm_resource_group.gauntlet.name
  storage_account_type = "Premium_LRS"
  create_option        = "Empty"
  disk_size_gb         = 64

  # Belt-and-braces matching the recovery drill's whole premise (further
  # down this doc): a `terraform destroy`, or an errant apply that would
  # replace this resource, must not be able to take history.db with it —
  # same intent as Phase 2's note that `az vm delete` leaves data disks
  # alone by default.
  lifecycle {
    prevent_destroy = true
  }
}

resource "azurerm_virtual_machine_data_disk_attachment" "gauntlet_state" {
  managed_disk_id    = azurerm_managed_disk.gauntlet_state.id
  virtual_machine_id = azurerm_linux_virtual_machine.gauntlet.id
  lun                = "0"
  caching            = "ReadWrite"
}
```

Two things this HCL does **not** do, on purpose:

- **Formatting/mounting the data disk** (Phase 2's `mkfs.ext4`/`blkid`/
  `fstab` steps) — Terraform provisions the disk, not its filesystem; run
  those commands over SSH after `apply` exactly as in the az CLI path,
  once, on first create.
- **Park/wake** — `azurerm_linux_virtual_machine` has no power-state
  attribute for Terraform to manage, so Phase 4's `az vm start`/`az vm
  deallocate` calls act purely at the runtime layer and cause **zero
  drift** on the next `terraform plan`. No `ignore_changes` block is
  needed for that reason; keep the timer-driven automation exactly as
  written in Phase 4 regardless of which provisioning path created the VM.

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
[deploy.md's exposure guidance](../deploy.md#dashboard--api--mcp-exposure-guidance));
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
mechanism docs/plans/scale.md §2 sketches. Two timer-driven pieces, either
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
# expect: an RFC3339 string after the queue has been empty a while, null/absent while busy
az vm get-instance-view -g <RESOURCE_GROUP> -n <VM_NAME> --query instanceView.statuses[1].displayStatus -o tsv
# expect: "VM deallocated" after a park, "VM running" after a wake
```

## Phase 5 — Upgrade procedure

```sh
URL=$(curl -fsSL https://api.github.com/repos/sgrankin/gauntlet/releases/latest \
  | grep -o '"browser_download_url": *"[^"]*linux_amd64\.tar\.gz"' \
  | cut -d'"' -f4)
ssh <ADMIN_USER>@<VM_IP> "curl -fsSL '$URL' | sudo tar -xz -C /usr/local/bin gauntlet && sudo systemctl restart gauntlet"
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
     --size Standard_D4s_v5 \
     --admin-username <ADMIN_USER> \
     --ssh-key-values ~/.ssh/id_ed25519.pub \
     --custom-data first-boot.sh \
     --public-ip-sku Standard
   ```

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
   ```

   Then run [verify.md](verify.md)'s full checklist — a candidate landing
   end-to-end after a from-scratch VM recreate is the real pass/fail signal
   for this whole drill, not any individual `az` command's exit code.

## Backup notes

Only `history.db` deserves separate backup thought — everything else under
`-state` (bare clones, `trials/`, `logs/`) is derivable, per
[deploy.md's "Backup notes"](../deploy.md#backup-notes): re-clonable from
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
