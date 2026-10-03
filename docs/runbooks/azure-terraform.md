# Azure VM Terraform alternative

## Terraform variant (provisioning phases 1–3)

If you'd rather provision declaratively than run Phases 1–3's `az` commands
by hand, here's the same shape as Terraform (`azurerm` provider) — resource
group, minimal networking (vnet/subnet/NIC, NSG allowing SSH only — the
dashboard stays localhost-bound per
[deploy.md's exposure guidance](../operations/security.md#dashboard-api-and-mcp),
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
  # dashboard over an SSH tunnel or tailnet, never a public inbound rule.
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

resource "azurerm_linux_virtual_machine" "gauntlet" {
  name                   = "<VM_NAME>"
  resource_group_name    = azurerm_resource_group.gauntlet.name
  location               = azurerm_resource_group.gauntlet.location
  size                   = "Standard_D8s_v5"   # same sizing rationale as Phase 1
  zone                   = "1"                 # must match the Premium v2 data disk's zone below
  admin_username         = "<ADMIN_USER>"
  network_interface_ids  = [azurerm_network_interface.gauntlet.id]

  admin_ssh_key {
    username   = "<ADMIN_USER>"
    public_key = file("~/.ssh/id_ed25519.pub")
  }

  os_disk {
    caching = "ReadWrite"
    # disk is what boot + apt run from — recreated on every recovery, so its
    # speed is your recovery time. Still disposable; the data disk below is
    storage_account_type = "StandardSSD_LRS"
  }

  # Stock Ubuntu LTS marketplace image — Terraform has no alias shorthand
  source_image_reference {
    publisher = "Canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }

  custom_data = base64encode(file("${path.module}/first-boot.sh"))
}

# --- data disk (Phase 2, as HCL) ---

resource "azurerm_managed_disk" "gauntlet_state" {
  name                 = "<VM_NAME>-state"
  location             = azurerm_resource_group.gauntlet.location
  resource_group_name  = azurerm_resource_group.gauntlet.name
  # per-GB as caches accumulate. Zonal-only (must match the VM's zone);
  # tune disk_iops_read_write/disk_mbps_read_write here if checks ever get
  storage_account_type = "PremiumV2_LRS"
  zone                 = "1"
  create_option        = "Empty"
  disk_size_gb         = 512

  # down this doc): a `terraform destroy`, or an errant apply that would
  # replace this resource, must not be able to take history.db with it —
  # same intent as Phase 2's note that `az vm delete` leaves data disks
  lifecycle {
    prevent_destroy = true
  }
}

resource "azurerm_virtual_machine_data_disk_attachment" "gauntlet_state" {
  managed_disk_id    = azurerm_managed_disk.gauntlet_state.id
  virtual_machine_id = azurerm_linux_virtual_machine.gauntlet.id
  lun                = "0"
  caching = "None"
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
