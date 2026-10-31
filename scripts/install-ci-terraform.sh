#!/usr/bin/env bash
# CI console-only tests. Official release bytes, pinned independently of download.
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
version=1.15.9
sha256=76edd0b22d2f27d3d2e097cd793209646f719cf60f02ff3af626b07361137da1
# https://releases.hashicorp.com/terraform/1.15.9/terraform_1.15.9_SHA256SUMS
test "$(uname -s)" = Linux && test "$(uname -m)" = x86_64
grep -Eq 'required_version[[:space:]]*=[[:space:]]*"= 1\.15\.9"' "$root/infra/terraform/p1/versions.tf"
destination=${1:?usage: install-ci-terraform.sh DESTINATION}
mkdir -p -- "$destination"
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT
curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
  "https://releases.hashicorp.com/terraform/$version/terraform_${version}_linux_amd64.zip" \
  --output "$temporary/terraform.zip"
printf '%s  %s\n' "$sha256" "$temporary/terraform.zip" | sha256sum --check --status
unzip -q "$temporary/terraform.zip" terraform -d "$temporary"
install -m 0755 "$temporary/terraform" "$destination/terraform"
CHECKPOINT_DISABLE=1 "$destination/terraform" version -json |
  python3 -c 'import json,sys; v=json.load(sys.stdin); assert v["terraform_version"] == "1.15.9" and v["platform"] == "linux_amd64", v; print("Terraform 1.15.9 linux_amd64 release SHA256 verified")'
