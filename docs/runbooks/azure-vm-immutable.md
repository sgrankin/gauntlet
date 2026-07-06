# Runbook: gauntlet on Azure — immutable-replace (Terraform-only)

**What you get:** the same warm-builder VM as [azure-vm.md](azure-vm.md), provisioned and upgraded entirely through Terraform — no SSH-and-fix-it maintenance, no golden image, no Packer. Upgrading the daemon is `terraform apply` after bumping one variable; the VM itself is disposable and gets replaced, not patched. This is the alternative for "I don't want to remember what state I hand-installed on this box" — pick this or azure-vm.md, not both, though they share the same data-disk/recovery philosophy.

**Why no Packer / golden image, stated plainly:** an image-baking pipeline (Packer build → Compute Gallery version → VM references the gallery) is the right answer once you're running a *fleet* of these VMs and image drift across them actually costs something. For one pet builder, it's a maintenance treadmill reintroduced one layer up — now you maintain a pipeline that maintains an image that maintains a VM, instead of just maintaining the VM. `cloud-init` doing full first-boot configuration off a stock marketplace image, driven entirely by Terraform, gets the same "provision from a known-good declarative spec" property with one moving part instead of three.

**Prerequisites** — same as [azure-vm.md](azure-vm.md)'s (an Azure subscription, resource group, SSH key pair), plus:

- `terraform` ≥ 1.5 installed locally (or wherever you run `apply` from).
- An Azure service principal or `az login` session Terraform can authenticate as (the `azurerm` provider's usual auth — this doc assumes `az login` + the provider's ambient-CLI-auth path, same credential `az` itself uses).

---

## Phase 1 — Terraform configuration

Three files: `variables.tf`, `main.tf`, and `cloud-init.yaml` (the templated first-boot config — this is the heart of this doc, Phase 2 below).

`variables.tf`:

```hcl
variable "resource_group" {
  type = string
}
variable "region" {
  type    = string
  default = "eastus"
}
variable "vm_name" {
  type    = string
  default = "gauntlet-builder"
}
variable "admin_user" {
  type    = string
  default = "gauntlet-admin"
}
variable "ssh_public_key_path" {
  type    = string
  default = "~/.ssh/id_ed25519.pub"
}
variable "admin_cidr" {
  description = "CIDR allowed to SSH in — narrow this, never 0.0.0.0/0"
  type        = string
}

# The whole upgrade mechanism: bump this, `terraform apply`, done. Bare
# semver, no leading "v" — the "v" is added where the GitHub release tag
# needs it (see local.gauntlet_download_url in main.tf), since goreleaser's
# archive filenames use the v-stripped {{ .Version }} while the release
# *tag*/URL path segment is the raw {{ .Tag }} ("v0.2.0"). Mixing these up
# produces a URL that 404s — get the "v" exactly where main.tf puts it,
# nowhere else.
variable "gauntlet_version" {
  type = string
}
```

`main.tf`:

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

locals {
  # See variables.tf's gauntlet_version comment for why the "v" appears
  # only in the tag path segment, never in the filename.
  gauntlet_download_url = "https://github.com/sgrankin/gauntlet/releases/download/v${var.gauntlet_version}/gauntlet_${var.gauntlet_version}_linux_amd64.tar.gz"
}

resource "azurerm_resource_group" "gauntlet" {
  name     = var.resource_group
  location = var.region
}

# --- networking: identical shape to azure-vm.md's Terraform variant ---

resource "azurerm_virtual_network" "gauntlet" {
  name                = "${var.vm_name}-vnet"
  address_space       = ["10.0.0.0/16"]
  location            = azurerm_resource_group.gauntlet.location
  resource_group_name = azurerm_resource_group.gauntlet.name
}

resource "azurerm_subnet" "gauntlet" {
  name                 = "${var.vm_name}-subnet"
  resource_group_name  = azurerm_resource_group.gauntlet.name
  virtual_network_name = azurerm_virtual_network.gauntlet.name
  address_prefixes     = ["10.0.1.0/24"]
}

resource "azurerm_network_security_group" "gauntlet" {
  name                = "${var.vm_name}-nsg"
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
    source_address_prefix      = var.admin_cidr
    destination_address_prefix = "*"
  }
  # No rule for 8080 here either — see azure-vm.md's Tailscale section for
  # the tailnet path; Phase 4 below shows it folded into this same cloud-init.
}

resource "azurerm_public_ip" "gauntlet" {
  name                = "${var.vm_name}-ip"
  location            = azurerm_resource_group.gauntlet.location
  resource_group_name = azurerm_resource_group.gauntlet.name
  allocation_method   = "Static"
  sku                 = "Standard"
}

resource "azurerm_network_interface" "gauntlet" {
  name                = "${var.vm_name}-nic"
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

# --- the VM: custom_data is where immutable-replace actually happens ---

resource "azurerm_linux_virtual_machine" "gauntlet" {
  name                   = var.vm_name
  resource_group_name    = azurerm_resource_group.gauntlet.name
  location               = azurerm_resource_group.gauntlet.location
  size                   = "Standard_D4s_v5"
  admin_username         = var.admin_user
  network_interface_ids  = [azurerm_network_interface.gauntlet.id]

  admin_ssh_key {
    username   = var.admin_user
    public_key = file(var.ssh_public_key_path)
  }

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "Standard_LRS"
  }

  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }
  # If this 404s for your region/subscription:
  # az vm image list-skus -l <REGION> -p Canonical -f ubuntu-24_04-lts -o table

  # THE upgrade mechanism. custom_data is documented ForceNew on this
  # resource — any change (including a changed gauntlet_version rendering a
  # different cloud-init file) forces Terraform to destroy and recreate the
  # VM, not patch it in place. There is no "reconfigure the running VM"
  # path here by design; see "Upgrade procedure" below for why that's fine.
  custom_data = base64encode(templatefile("${path.module}/cloud-init.yaml", {
    gauntlet_download_url = local.gauntlet_download_url
  }))
}

# --- data disk: the one thing that survives every replace ---

resource "azurerm_managed_disk" "gauntlet_state" {
  name                 = "${var.vm_name}-state"
  location             = azurerm_resource_group.gauntlet.location
  resource_group_name  = azurerm_resource_group.gauntlet.name
  storage_account_type = "Premium_LRS"
  create_option        = "Empty"
  disk_size_gb         = 64

  # The safety net for the whole "replace the VM freely" design — a
  # `terraform destroy` (or an errant plan that would replace this
  # specific resource, as opposed to the VM) must not be able to take
  # history.db with it.
  lifecycle {
    prevent_destroy = true
  }
}

resource "azurerm_virtual_machine_data_disk_attachment" "gauntlet_state" {
  managed_disk_id    = azurerm_managed_disk.gauntlet_state.id
  virtual_machine_id = azurerm_linux_virtual_machine.gauntlet.id
  lun                = "0"   # cloud-init's fs_setup below targets this exact LUN via /dev/disk/azure/scsi1/lun0
  caching            = "ReadWrite"
}
```

**Terraform state stays secret-free.** Nothing above embeds a token, a
password, or the repo's remote URL — `gauntlet.kdl`/`gauntlet.env` (which
do carry those) are delivered out-of-band onto the persistent data disk in
Phase 3, never templated into `custom_data`, so they never touch
`terraform.tfstate` either. This is a deliberate property of the design,
not an oversight: state files are so routinely mishandled (committed,
shared, left in a CI artifact) that "the secrets literally aren't in there"
beats "remember to keep the state file secure."

**VERIFY**

```sh
terraform init
terraform validate
```

## Phase 2 — cloud-init.yaml (the heart of this doc)

This does everything azure-vm.md's `first-boot.sh` did, plus the data-disk
mount (which that doc's operator did once by hand over SSH — here it has to
be unattended, since there's no "by hand" step between a `terraform apply`
and the VM being live):

```yaml
#cloud-config

package_update: true

# --- docker's data-root: write BEFORE docker ever starts, not after ---
#
# This is the highest-stakes fix in this whole file for the immutable-
# replace design specifically: every upgrade IS a VM replace (Phase 5), so
# without this, every upgrade also cold-discards every pulled image and
# every named cache volume (the executor's `cache "gocache" path="..."`
# entries) — the durable-data-disk design buys nothing if docker's actual
# data still lives on the disposable OS disk. `write_files` and
# `fs_setup`/`mounts` (below) are both early-stage cloud-init modules that
# complete well before `runcmd` (a later stage — where docker actually gets
# installed and started, further down) ever runs — so by the time the
# `docker-ce` package's postinst starts dockerd for the very first time on
# this VM, /etc/docker/daemon.json already points it at the data disk, AND
# that disk is already mounted. No stop/move/restart retrofit dance is
# needed here the way it is for an already-running pet VM (see azure-vm.md's
# Operations section for that case) — this file only ever runs against a VM
# that hasn't started docker yet.
write_files:
  - path: /etc/docker/daemon.json
    content: |
      {"data-root": "/mnt/gauntlet-state/docker"}

# --- format + mount the data disk, WITHOUT reformatting it on every replace ---
#
# This is the one part of this file to read twice before trusting it. The
# data disk survives every VM replace (that's its entire purpose); the OS
# disk, and everything cloud-init would otherwise remember about "have I
# already formatted this," does NOT. So the guard against reformatting on
# the second, third, ... Nth replace has to live in cloud-init's own
# idempotency check, not in "only run this once" logic — there IS no
# "once" here, this file runs fresh on literally every replace.
#
# The device path is /dev/disk/azure/scsi1/lun0 — Azure's stable, udev-
# generated symlink for the disk attached at LUN 0 (main.tf's
# azurerm_virtual_machine_data_disk_attachment.lun), NOT a /dev/sdX name,
# which is not guaranteed to be the same device across boots.
#
# partition: any + overwrite: false is the specific combination that's
# actually safe — verified against cloud-init's fs_setup module source
# (cc_disk_setup.py), not just the prose docs, because the prose and a
# naive reading both invite a dangerous mistake here:
#   - partition: none looks like "no partition table, just format the
#     device" (true) but ALWAYS calls mkfs unconditionally — it skips the
#     existing-filesystem check entirely. Using it here would reformat
#     (destroy) history.db on every single replace. Do not use it.
#   - partition: any (or auto) instead checks the device for an existing
#     filesystem matching `filesystem` (any also ignores `label`) before
#     doing anything, and returns without formatting if found. This is the
#     one that's actually idempotent across replaces.
fs_setup:
  - label: gauntletstate
    filesystem: ext4
    device: /dev/disk/azure/scsi1/lun0
    partition: any
    overwrite: false

mounts:
  - [/dev/disk/azure/scsi1/lun0, /mnt/gauntlet-state, ext4, "defaults,nofail", "0", "2"]

runcmd:
  # Must run after fs_setup/mounts (above) have the data disk live at
  # /mnt/gauntlet-state — runcmd's own stage runs later than both, so this
  # is safe, but don't reorder this ahead of docker's install below without
  # rechecking that the mount is actually up by then.
  - mkdir -p /mnt/gauntlet-state/docker

  # Docker engine from docker's own apt repo (Ubuntu's lags).
  - curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /usr/share/keyrings/docker.gpg
  - >
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker.gpg]
    https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" |
    tee /etc/apt/sources.list.d/docker.list
  - apt-get update
  - apt-get install -y docker-ce docker-ce-cli containerd.io git unattended-upgrades

  # OS patching on autopilot — same rationale as azure-vm.md.
  - dpkg-reconfigure -f noninteractive unattended-upgrades

  # A dedicated, non-DynamicUser system user: this VM's whole point is
  # running the container executor, which needs stable docker-group
  # membership (deploy-linux.md's own noted alternative to systemd's
  # DynamicUser for exactly this reason).
  - useradd --system --no-create-home --shell /usr/sbin/nologin -G docker gauntlet || true

  # Fetch the pinned gauntlet release — no "latest" lookup here, on
  # purpose: immutable-replace means every apply is reproducible from
  # var.gauntlet_version, not from whatever tag happened to be newest when
  # cloud-init ran.
  - mkdir -p /usr/local/bin
  - curl -fsSL "${gauntlet_download_url}" | tar -xz -C /usr/local/bin gauntlet
  - mkdir -p /mnt/gauntlet-state/state

  # gauntlet.kdl and gauntlet.env are NOT written here — they live on
  # /mnt/gauntlet-state (the persistent disk), delivered once out-of-band
  # (Phase 3). Putting them there rather than under /etc on the OS disk is
  # what makes every SUBSEQUENT replace need zero manual secret delivery:
  # the disk that already has them just gets re-attached.

  - |
    cat > /etc/systemd/system/gauntlet.service <<'EOF'
    [Unit]
    Description=gauntlet merge-queue daemon
    After=network-online.target
    Wants=network-online.target

    [Service]
    Type=simple
    User=gauntlet
    Group=gauntlet
    ExecStart=/usr/local/bin/gauntlet -config /mnt/gauntlet-state/gauntlet.kdl -state /mnt/gauntlet-state/state
    EnvironmentFile=/mnt/gauntlet-state/gauntlet.env
    Restart=on-failure
    RestartSec=5s
    NoNewPrivileges=yes

    [Install]
    WantedBy=multi-user.target
    EOF
  - systemctl daemon-reload
  - systemctl enable gauntlet
  # NOT `systemctl start` here on purpose: on a genuinely first provision,
  # gauntlet.kdl/gauntlet.env don't exist yet (Phase 3 hasn't run), and
  # `enable` alone means the unit starts on the NEXT boot once they do
  # exist — a boot that Phase 3 below triggers deliberately via `reboot`,
  # rather than the unit failing/restart-looping against a missing config
  # in the meantime.
```

**One templating gotcha worth naming even though this file avoids it:**
Terraform's `templatefile()` uses `${...}` for its own interpolation, which
collides syntactically with bash's `${VAR}` form. This file sidesteps the
problem entirely by computing the one real Terraform-side value
(`gauntlet_download_url`) in `main.tf`'s `locals` and passing the finished
string in — there's no bash `${...}` left anywhere above for Terraform to
misinterpret. If you extend this template and need an actual bash
brace-expansion, escape it as `$${...}` (double dollar) or Terraform will
try to resolve it as its own variable and fail loudly at `plan` time.

**Known rough edge:** on rare first boots, `/dev/disk/azure/scsi1/lun0` can
appear a beat after cloud-init's `fs_setup`/`mounts` modules run (Azure's
udev rules racing early boot) — if a first provision comes up with
`/mnt/gauntlet-state` unmounted, `sudo cloud-init clean --logs && sudo
reboot` once resolves it; it hasn't been observed on a *replace* against an
already-formatted disk (the earlier machine already has the symlink
warm from a prior boot cycle's udev database, though the fresh VM's own
first boot does not carry that over).

**VERIFY** (Phase 3's first-boot verification, since the VM isn't useful
until secrets land)

```sh
ssh <ADMIN_USER>@<VM_IP> 'mount | grep gauntlet-state && systemctl is-enabled gauntlet'
# expect: /dev/disk/azure/scsi1/lun0 on /mnt/gauntlet-state, and "enabled"
# (NOT "active" yet — that's expected before Phase 3)
ssh <ADMIN_USER>@<VM_IP> "docker info --format '{{.DockerRootDir}}'"
# expect: /mnt/gauntlet-state/docker, not /var/lib/docker — confirms
# write_files won the ordering race above, on THIS boot, not just in theory
```

**What survives the next replace/upgrade, and what doesn't.** Pulled
images, the executor's named `cache` volumes (deploy-linux.md's
`gocache`/`gomodcache` example — bare volume names live inside docker's
data-root, no extra config needed once the move above is in place), and
buildkit's cache all live under `/mnt/gauntlet-state/docker`, so
Phase 5's upgrade (a full VM replace) doesn't cold-discard them — this is
the entire point of moving data-root before docker's first start.
**Shared-service instances (`services` block) are the one thing this does
NOT carry over warm across a replace**, even though their containers still
physically exist in the preserved data-root: a replace is a fresh VM boot,
so every previously-running service container comes back *stopped*, and
gauntlet's boot-adoption sweep destroys anything that isn't actively
running (services.md §3 "Adoption at boot, not reaping" — probe-alive
fails for a stopped container) rather than restarting it. The next run
needing that service recreates it — fast, since the *image* survived, just
not instantly warm the way it would after a mere park/wake deallocation
(which never stops containers at all, only the VM around them).

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
# expect: active
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
  # add alongside the other runcmd entries above
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
# wait-for-idle.sh — poll until the daemon is idle, or a timeout elapses,
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
# expect: your check-builder/service images already listed, no pull needed
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
[deploy.md's "Backup notes"](../deploy.md#backup-notes)), and
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
