resource "azurerm_resource_group" "testbed" {
  name     = "${var.name_prefix}-rg"
  location = var.location
  tags     = var.tags

  lifecycle {
    precondition {
      condition     = var.deployment_prerequisites_confirmed
      error_message = "Deployment is gated until the operator verifies provider registration, subscription/policy/RBAC, regional and ARM64 family quota, SKU/image/disk compatibility, reviewed CIDRs, cost/cleanup ownership and remote state readiness. See docs/runbooks/azure-arm64-deployment.md. The attestation does not authorize deployment."
    }
  }
}

resource "azurerm_virtual_network" "testbed" {
  name                = "${var.name_prefix}-vnet"
  location            = azurerm_resource_group.testbed.location
  resource_group_name = azurerm_resource_group.testbed.name
  address_space       = [var.vnet_cidr]
  tags                = var.tags
}

resource "azurerm_subnet" "worker" {
  name                            = "${var.name_prefix}-worker"
  resource_group_name             = azurerm_resource_group.testbed.name
  virtual_network_name            = azurerm_virtual_network.testbed.name
  address_prefixes                = [var.subnet_cidr]
  default_outbound_access_enabled = false
}

resource "azurerm_network_security_group" "worker" {
  name                = "${var.name_prefix}-nsg"
  location            = azurerm_resource_group.testbed.location
  resource_group_name = azurerm_resource_group.testbed.name
  tags                = var.tags

  dynamic "security_rule" {
    for_each = var.bootstrap_ssh_source_cidr == null ? [] : [var.bootstrap_ssh_source_cidr]
    content {
      name                       = "AllowOperatorBootstrapSSH"
      priority                   = 100
      direction                  = "Inbound"
      access                     = "Allow"
      protocol                   = "Tcp"
      source_port_range          = "*"
      destination_port_range     = "22"
      source_address_prefix      = security_rule.value
      destination_address_prefix = "*"
    }
  }

  # Override built-in AllowVnetInBound and AllowAzureLoadBalancerInBound too.
  security_rule {
    name                       = "DenyAllInbound"
    priority                   = 200
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }
  # Stateful outbound replies are allowed. Tailscale can use outbound relay;
  # no public Kubernetes or Tailscale inbound ports are opened here.
}

resource "azurerm_subnet_network_security_group_association" "worker" {
  subnet_id                 = azurerm_subnet.worker.id
  network_security_group_id = azurerm_network_security_group.worker.id
}

resource "azurerm_public_ip" "worker_egress" {
  name                = "${var.name_prefix}-egress-pip"
  location            = azurerm_resource_group.testbed.location
  resource_group_name = azurerm_resource_group.testbed.name
  allocation_method   = "Static"
  sku                 = "Standard"
  ip_version          = "IPv4"
  tags                = var.tags
}

resource "azurerm_network_interface" "worker" {
  name                           = "${var.name_prefix}-nic"
  location                       = azurerm_resource_group.testbed.location
  resource_group_name            = azurerm_resource_group.testbed.name
  accelerated_networking_enabled = true
  ip_forwarding_enabled          = false
  tags                           = var.tags

  ip_configuration {
    name                          = "primary"
    primary                       = true
    subnet_id                     = azurerm_subnet.worker.id
    private_ip_address_allocation = "Dynamic"
    public_ip_address_id          = azurerm_public_ip.worker_egress.id
  }

  # Avoid attaching a reachable NIC before its subnet has the restrictive NSG.
  depends_on = [azurerm_subnet_network_security_group_association.worker]
}

resource "azurerm_linux_virtual_machine" "worker" {
  name                            = "${var.name_prefix}-arm-worker"
  location                        = azurerm_resource_group.testbed.location
  resource_group_name             = azurerm_resource_group.testbed.name
  size                            = var.vm_size
  admin_username                  = var.admin_username
  disable_password_authentication = true
  network_interface_ids           = [azurerm_network_interface.worker.id]
  tags                            = var.tags

  admin_ssh_key {
    username   = var.admin_username
    public_key = trimspace(var.ssh_public_key)
  }

  # Managed OS disk is owned inline by the VM; no duplicate managed_disk.
  os_disk {
    name                 = "${var.name_prefix}-osdisk"
    caching              = "ReadWrite"
    storage_account_type = "StandardSSD_LRS"
    disk_size_gb         = var.os_disk_size_gb
  }

  source_image_reference {
    publisher = var.ubuntu_image.publisher
    offer     = var.ubuntu_image.offer
    sku       = var.ubuntu_image.sku
    version   = var.ubuntu_image.version
  }
}

resource "azurerm_container_registry" "testbed" {
  name                          = var.acr_name
  location                      = azurerm_resource_group.testbed.location
  resource_group_name           = azurerm_resource_group.testbed.name
  sku                           = var.acr_sku
  admin_enabled                 = false
  public_network_access_enabled = true
  anonymous_pull_enabled        = false
  tags                          = var.tags
}
