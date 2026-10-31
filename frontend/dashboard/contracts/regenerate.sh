#!/usr/bin/env bash
# Export either immutable historical or current backend contracts from a Git archive.
set -euo pipefail
backend=${1:?Usage: bash contracts/regenerate.sh /path/to/backend-git-repository [historical|current] [output-directory]}
mode=${2:-historical}
contract_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
case "$mode" in
  historical) ref=b4fb78d0707afdbc93867d087fea5d1511203208; fixtures="$contract_dir/../src/domain/__fixtures__" ;;
  current) ref=HEAD; fixtures="$contract_dir/../src/domain/__fixtures__/current" ;;
  *) echo "Unknown contract version: $mode" >&2; exit 2 ;;
esac
output=${3:-$fixtures}
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/opsloop-dashboard-contract.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
umask 077
git -C "$backend" archive "$ref" | tar -x -C "$scratch"
cp "$contract_dir/export_test.go.txt" "$scratch/internal/candidates/dashboard_contract_test.go"
DASHBOARD_CONTRACT_OUT="$output/go-candidates.json" \
DASHBOARD_QUANTITY_OUT="$output/go-quantities.json" \
go -C "$scratch" test ./internal/candidates -run '^TestDashboard(Contract|Quantity)Export$' -count=1
