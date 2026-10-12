# P1-A local review only: no backend, plan, apply or bootstrap in this phase.
# Terraform matches the installed/validated CLI; AzureRM is a deliberate exact
# reproducibility pin, not a claim to use the newest available provider.
# Release: https://github.com/hashicorp/terraform-provider-azurerm/releases/tag/v4.64.0
# Provider registration contract:
# https://github.com/hashicorp/terraform-provider-azurerm/blob/v4.64.0/website/docs/index.html.markdown
# Subnet/NIC/VM schema references are under the same release's website/docs/r/.
# A remote backend must be independently prepared and reviewed before any
# future state-producing operation. No storage resources are owned here.
terraform {
  required_version = "= 1.15.9"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "= 4.64.0"
    }
  }
}

provider "azurerm" {
  features {}
  subscription_id                 = var.subscription_id
  resource_provider_registrations = "none"
  resource_providers_to_register  = []
}
