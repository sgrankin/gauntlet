# Azure VM setup

**What you get:** a single "pet" Ubuntu VM running the `gauntlet` binary
directly (deploy-linux.md's Topology (a), on Azure) — no golden image, no
Packer, no Compute Gallery; state lives on a separate data disk so the VM
itself is disposable and recreatable in minutes. This doc covers the Azure
layer only — provisioning, disks, park/wake, upgrade, and recovery. For the
daemon config, systemd unit, GitHub/Slack setup, and first-run verification,
see [deploy-linux.md](deploy-linux.md) — this runbook references its phases
rather than repeating them.

Use [immutable replacement](azure-vm-immutable.md) for Terraform-driven upgrades
instead of maintaining the host over SSH.

**Prerequisites**

- `az` CLI logged in (`az login`) with a subscription selected
  (`az account set --subscription <SUBSCRIPTION_ID>`).
- A resource group (`az group create -n <RESOURCE_GROUP> -l <REGION>`).
- An SSH key pair (`~/.ssh/id_ed25519[.pub]` or equivalent) for VM access.
- Everything deploy-linux.md's own prerequisites list (git ≥2.40 ships with
  Ubuntu LTS already; docker is installed by the first-boot script below,
  not assumed present).

---

Alternatively use the [Terraform setup](azure-terraform.md). After provisioning, follow [Azure operations](azure-operations.md).

## Phase 1 — Provision the VM

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

- **Sizing:** `Standard_D8s_v5` (8 vCPU / 32 GiB RAM) is the recommendation
  for a builder whose test suite parallelizes aggressively — a heavily
  parallel `go test` run saturates 8 cores on its own, before SQL Server and
  the docker daemon take their share (checks.md's
  ["Shared services"](../reference/services.md#shared-services) wants headroom beyond
  the containers' own `memory`/`cpus` ceilings). `D4s_v5` (4/16) is the
  floor for anything running SQL-Server-class services at all; if wall-clock
  timings show sustained 8-core saturation, `D16s_v5` is the next notch —
  measure before paying for it. Ballpark cost (East US PAYG, drifts): D8s
  ≈ $280/mo always-on, ≈ $84/mo parked outside ~220 work-hours; D4s is half
  that; storage below is ~$45/mo either way.
- `--zone 1`: the VM must be **zonal** (any zone, but pinned) because the
  Premium SSD **v2** data disk below is zonal-only and must live in the same
  zone as the VM.
- **No spot instance, on purpose** — spot eviction is exactly the kind of
  infra error `auto-retry-errors` (README's ["Retry
  semantics"](../guides/landing.md), already shipped) exists to
  absorb once, but stacking spot eviction *and* the park/wake deallocation
  below multiplies infra-error surface for no real savings at this scale
  ([design/scaling.md](../architecture/scaling.md)'s own framing: revisit spot only
  once a remote-executor/multi-worker story exists — not before).
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
ssh <ADMIN_USER>@$(az vm show -g <RESOURCE_GROUP> -n <VM_NAME> -d --query publicIps -o tsv) echo ok
```

## Phase 2 — Attach the data disk

The OS disk is disposable (recreate from the stock image anytime). Everything
that needs to survive a VM recreate — the daemon's `-state` dir, most
importantly `history.db` — lives on a **separate** managed disk instead.

```sh
az disk create \
  --resource-group <RESOURCE_GROUP> \
  --name <VM_NAME>-state \
  --size-gb 512 \
  --sku PremiumV2_LRS \
  --zone 1

az vm disk attach \
  --resource-group <RESOURCE_GROUP> \
  --vm-name <VM_NAME> \
  --name <VM_NAME>-state
```

- **Premium SSD v2**, not v1: per-GB billing (≈ $42/mo at 500 GiB vs ≈ $66
  for a P20) *and* a higher included baseline (3000 IOPS / 125 MBps, tunable
  upward for money) — the better deal at this size, and it grows per-GB as
  build caches accumulate instead of jumping provisioned tiers. Two
  constraints it imposes, both already handled here: it is **zonal-only**
  (`--zone` must match the VM's) and it does **not support host caching**
  (attach with default/`None` caching — do not request ReadWrite). If your
  region/zone lacks v2, fall back to `--sku Premium_LRS` (tiered, host
  caching allowed).

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

# Docker's data-root MUST move onto the data disk BEFORE docker's first
# lands on the disposable OS disk instead, and a VM
# OS disk once, moving it later is the retrofit dance in "Operations"
sudo mkdir -p /mnt/gauntlet-state/docker
sudo mkdir -p /etc/docker
echo '{"data-root": "/mnt/gauntlet-state/docker"}' | sudo tee /etc/docker/daemon.json

curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /usr/share/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io git unattended-upgrades

sudo dpkg-reconfigure -f noninteractive unattended-upgrades

URL=$(curl -fsSL https://api.github.com/repos/sgrankin/gauntlet/releases/latest \
  | grep -o '"browser_download_url": *"[^"]*gauntlet_linux_amd64"' \
  | cut -d'"' -f4)
sudo curl -fsSL -o /usr/local/bin/gauntlet "$URL"
sudo chmod +x /usr/local/bin/gauntlet

sudo mkdir -p /etc/gauntlet
sudo mkdir -p /mnt/gauntlet-state/state
# out-of-band (they carry your remote URL and secrets); see deploy-linux.md
```

Then install the systemd unit exactly as deploy-linux.md's Phase 5, with
one change — `ExecStart`'s `-state` points at the mounted data disk:

```ini
ExecStart=/usr/local/bin/gauntlet -config /etc/gauntlet/gauntlet.kdl -state /mnt/gauntlet-state/state
```

**VERIFY** — run deploy-linux.md's Phase 5 VERIFY (`systemctl is-active
gauntlet`, `journalctl -u gauntlet -n 30`), then the full
[verify.md](verify.md) checklist once, end to end. Also confirm docker
actually took the relocated data-root before you rely on it surviving a
recreate:

```sh
docker info --format '{{.DockerRootDir}}'
```

**What this preserves across a VM recreate, and what it doesn't.** Pulled
images, the executor's named `cache` volumes (`gocache`/`gomodcache` in
deploy-linux.md's example — bare volume names live inside docker's
data-root, so the move above covers them with no config change needed),
and buildkit's own cache all live under `/mnt/gauntlet-state/docker` now,
so they survive a recreate intact — no cold re-pull, no cold module cache.
**Warm shared-service instances (`services` block) do not carry over the
same way**, even though their containers technically still exist on disk:
a VM recreate is a fresh boot, so every previously-running container comes
back *stopped*, and gauntlet's own boot-time adoption sweep destroys any
service instance that isn't actively running (services.md §3 "Adoption at
boot, not reaping" — probe-alive fails for a stopped container, so it's
treated as unmatched and torn down) rather than restarting it. The next run
needing that service recreates it fresh — fast, because the *image* is
still warm, just not instant the way a merely-park/waked VM (which never
stops its containers) would be. Only a VM **recreate** (this phase, or the
recovery drill) goes through this; ordinary park/wake deallocation leaves
containers exactly as they were.

**Operations note — retrofitting an already-provisioned VM** that installed
docker before this data-root relocation existed:

```sh
sudo systemctl stop docker
# free space on the data disk for the copy):
sudo mkdir -p /mnt/gauntlet-state/docker
sudo rsync -a /var/lib/docker/ /mnt/gauntlet-state/docker/
sudo mkdir -p /etc/docker
echo '{"data-root": "/mnt/gauntlet-state/docker"}' | sudo tee /etc/docker/daemon.json
sudo systemctl start docker
docker info --format '{{.DockerRootDir}}'   # VERIFY: /mnt/gauntlet-state/docker
```
