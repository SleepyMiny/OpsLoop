#!/usr/bin/env bash
# Read the pinned backend from a local Git object database; never checkout/merge it.
set -euo pipefail
backend=${1:?Usage: bash contracts/regenerate.sh /path/to/backend-git-repository}
ref=b4fb78d0707afdbc93867d087fea5d1511203208
contract_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/opsloop-dashboard-contract.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
umask 077
git -C "$backend" archive "$ref" | tar -x -C "$scratch"
cp "$contract_dir/export_test.go" "$scratch/internal/candidates/dashboard_contract_test.go"
DASHBOARD_CONTRACT_OUT="$contract_dir/../src/domain/__fixtures__/go-candidates.json" \
DASHBOARD_QUANTITY_OUT="$contract_dir/../src/domain/__fixtures__/go-quantities.json" \
go -C "$scratch" test ./internal/candidates -run '^TestDashboard(Contract|Quantity)Export$' -count=1
