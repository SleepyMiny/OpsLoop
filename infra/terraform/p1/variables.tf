variable "deployment_prerequisites_confirmed" {
  description = "Keep false until P1-B verifies provider registration, regional ARM64 SKU/quota/image compatibility and deployment prerequisites. Setting true is an operator attestation, not permission to deploy."
  type        = bool
  default     = false
  nullable    = false
}

variable "subscription_id" {
  description = "Nonsecret target subscription UUID; no credentials belong in Terraform inputs."
  type        = string
  nullable    = false
  validation {
    condition     = can(regex("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$", var.subscription_id))
    error_message = "subscription_id must be a UUID."
  }
}

variable "location" {
  description = "Required Azure region selected after subscription SKU, quota and image checks."
  type        = string
  nullable    = false
}

variable "name_prefix" {
  type    = string
  default = "opsloop-p1"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,23}$", var.name_prefix))
    error_message = "Use 3-24 lowercase letters, digits or hyphens, starting with a letter."
  }
}

variable "vm_size" {
  description = "Preferred ARM64 candidate; availability and alternate SKU compatibility are not guaranteed."
  type        = string
  default     = "Standard_D2pls_v6"
  validation {
    condition = contains([
      "Standard_D2pls_v6",
      "Standard_D2pls_v5",
      "Standard_D2plds_v6",
      "Standard_D2ps_v6",
      "Standard_D2ps_v5",
    ], var.vm_size)
    error_message = "Select an ARM64 candidate: Standard_D2pls_v6, Standard_D2pls_v5, Standard_D2plds_v6, Standard_D2ps_v6 or Standard_D2ps_v5; regional availability still requires verification."
  }
}

variable "ubuntu_image" {
  description = "Verified Canonical Ubuntu ARM64 image reference. All fields including exact version are required."
  type = object({
    publisher = string
    offer     = string
    sku       = string
    version   = string
  })
  nullable = false
  validation {
    condition = (
      var.ubuntu_image.publisher == "Canonical" &&
      can(regex("arm64", var.ubuntu_image.sku)) &&
      can(regex("^[0-9]+[.][0-9]+[.][0-9]+$", var.ubuntu_image.version))
    )
    error_message = "Supply a verified Canonical ARM64 SKU and numeric major.minor.build version; latest is forbidden."
  }
}

variable "admin_username" {
  type    = string
  default = "opsloop"
}

variable "ssh_public_key" {
  description = "Required nonsecret OpenSSH RSA public key (at least 2048 bits); never a private key or file path."
  type        = string
  nullable    = false
  validation {
    condition     = can(regex("^ssh-rsa [A-Za-z0-9+/]+={0,3}( .*)?$", trimspace(var.ssh_public_key)))
    error_message = "Supply an OpenSSH RSA public key; Azure Linux VM requires RSA of at least 2048 bits."
  }
}

variable "bootstrap_ssh_source_cidr" {
  description = "Optional operator public IPv4 /32. Null closes SSH; remove the exception after separately authorized bootstrap."
  type        = string
  default     = null
  validation {
    condition = var.bootstrap_ssh_source_cidr == null ? true : (
      can(cidrnetmask(var.bootstrap_ssh_source_cidr)) &&
      can(regex("/32$", var.bootstrap_ssh_source_cidr))
    )
    error_message = "Bootstrap SSH must be null or one IPv4 /32."
  }
}

variable "vnet_cidr" {
  description = "Proposed VNet range; changes require overlap review against LAN, Docker, tailnet, Pod and Service networks."
  type        = string
  default     = "10.80.0.0/16"
  validation {
    condition     = can(cidrnetmask(var.vnet_cidr))
    error_message = "Use an IPv4 CIDR."
  }
}

variable "subnet_cidr" {
  type    = string
  default = "10.80.1.0/24"
  validation {
    condition     = can(cidrnetmask(var.subnet_cidr))
    error_message = "Use an IPv4 CIDR within the VNet."
  }
}

variable "os_disk_size_gb" {
  type    = number
  default = 32
  validation {
    condition     = var.os_disk_size_gb >= 30 && floor(var.os_disk_size_gb) == var.os_disk_size_gb
    error_message = "Disk size must be a whole number of GiB >= 30 and at least the selected image minimum."
  }
}

variable "acr_sku" {
  description = "ACR tier; Basic is the P1 MVP default. Higher tiers require separate cost and prerequisite review."
  type        = string
  default     = "Basic"
  nullable    = false
  validation {
    condition     = contains(["Basic", "Standard", "Premium"], var.acr_sku)
    error_message = "ACR SKU must be Basic, Standard or Premium."
  }
}

variable "acr_name" {
  description = "Nonsecret globally unique ACR name; availability must be checked separately."
  type        = string
  nullable    = false
  validation {
    condition     = can(regex("^[a-zA-Z0-9]{5,50}$", var.acr_name))
    error_message = "ACR name must contain 5-50 alphanumeric characters."
  }
}

variable "tags" {
  type = map(string)
  default = {
    project     = "OpsLoop"
    environment = "testbed"
    phase       = "P1"
  }
}
