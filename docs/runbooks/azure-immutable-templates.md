# Immutable Azure VM templates

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
  gauntlet_download_url = "https://github.com/sgrankin/gauntlet/releases/download/v${var.gauntlet_version}/gauntlet_linux_amd64"
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

resource "azurerm_linux_virtual_machine" "gauntlet" {
  name                   = var.vm_name
  resource_group_name    = azurerm_resource_group.gauntlet.name
  location               = azurerm_resource_group.gauntlet.location
  size                   = "Standard_D8s_v5"
  zone                   = "1" # must match the Premium v2 data disk's zone
  admin_username         = var.admin_user
  network_interface_ids  = [azurerm_network_interface.gauntlet.id]

  admin_ssh_key {
    username   = var.admin_user
    public_key = file(var.ssh_public_key_path)
  }

  os_disk {
    caching = "ReadWrite"
    # StandardSSD, not Standard_LRS (spinning HDD): this disk is recreated
    storage_account_type = "StandardSSD_LRS"
  }

  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }

  # different cloud-init file) forces Terraform to destroy and recreate the
  custom_data = base64encode(templatefile("${path.module}/cloud-init.yaml", {
    gauntlet_download_url = local.gauntlet_download_url
  }))
}

# --- data disk: the one thing that survives every replace ---

resource "azurerm_managed_disk" "gauntlet_state" {
  name                 = "${var.vm_name}-state"
  location             = azurerm_resource_group.gauntlet.location
  resource_group_name  = azurerm_resource_group.gauntlet.name
  # Zonal-only (must match the VM's zone). Tune disk_iops_read_write /
  # disk_mbps_read_write if checks ever get IO-bound. Fall back to
  storage_account_type = "PremiumV2_LRS"
  zone                 = "1"
  create_option        = "Empty"
  disk_size_gb         = 512

  # The safety net for the whole "replace the VM freely" design — a
  # `terraform destroy` (or an errant plan that would replace this
  # specific resource, as opposed to the VM) must not be able to take
  lifecycle {
    prevent_destroy = true
  }
}

resource "azurerm_virtual_machine_data_disk_attachment" "gauntlet_state" {
  managed_disk_id    = azurerm_managed_disk.gauntlet_state.id
  virtual_machine_id = azurerm_linux_virtual_machine.gauntlet.id
  lun                = "0"   # cloud-init's fs_setup below targets this exact LUN via /dev/disk/azure/scsi1/lun0
  # disk's own 3000-IOPS baseline is what serves the git/docker/build bulk.
  caching = "None"
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

#
# entries) — the durable-data-disk design buys nothing if docker's actual
# data still lives on the disposable OS disk. `write_files` and
# `fs_setup`/`mounts` (below) are both early-stage cloud-init modules that
# this VM, /etc/docker/daemon.json already points it at the data disk, AND
# that disk is already mounted. No stop/move/restart retrofit dance is
write_files:
  - path: /etc/docker/daemon.json
    content: |
      {"data-root": "/mnt/gauntlet-state/docker"}

# --- format + mount the data disk, WITHOUT reformatting it on every replace ---
#
# data disk survives every VM replace (that's its entire purpose); the OS
# disk, and everything cloud-init would otherwise remember about "have I
# already formatted this," does NOT. So the guard against reformatting on
#
# The device path is /dev/disk/azure/scsi1/lun0 — Azure's stable, udev-
# generated symlink for the disk attached at LUN 0 (main.tf's
# azurerm_virtual_machine_data_disk_attachment.lun), NOT a /dev/sdX name,
#
# actually safe — verified against cloud-init's fs_setup module source
# (cc_disk_setup.py), not just the prose docs, because the prose and a
#   - partition: none looks like "no partition table, just format the
#     existing-filesystem check entirely. Using it here would reformat
#     (destroy) history.db on every single replace. Do not use it.
#     doing anything, and returns without formatting if found. This is the
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
  # is safe, but don't reorder this ahead of docker's install below without
  # rechecking that the mount is actually up by then.
  - mkdir -p /mnt/gauntlet-state/docker

  - curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /usr/share/keyrings/docker.gpg
  - >
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker.gpg]
    https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" |
    tee /etc/apt/sources.list.d/docker.list
  - apt-get update
  - apt-get install -y docker-ce docker-ce-cli containerd.io git unattended-upgrades

  - dpkg-reconfigure -f noninteractive unattended-upgrades

  - useradd --system --no-create-home --shell /usr/sbin/nologin -G docker gauntlet || true

  - mkdir -p /usr/local/bin
  - curl -fsSL -o /usr/local/bin/gauntlet "${gauntlet_download_url}"
  - chmod +x /usr/local/bin/gauntlet
  - mkdir -p /mnt/gauntlet-state/state

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
ssh <ADMIN_USER>@<VM_IP> "docker info --format '{{.DockerRootDir}}'"
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
