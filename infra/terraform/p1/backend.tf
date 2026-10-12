# Partial configuration only. Backend storage is managed by a separate root and
# survives testbed destroy. Local checks MUST use init -backend=false.
# Supply only reviewed nonsecret identifiers with -backend-config; credentials
# stay in the operator's Azure CLI session, never backend HCL or Terraform inputs.
terraform {
  backend "azurerm" {
    use_azuread_auth = true
    use_cli          = true
  }
}
