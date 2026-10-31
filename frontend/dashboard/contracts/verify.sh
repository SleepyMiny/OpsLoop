#!/usr/bin/env bash
# Compare generated contracts; never rewrite committed fixtures to make CI pass.
set -euo pipefail
backend=${1:?Usage: bash contracts/verify.sh /path/to/backend-git-repository [historical|current]}
mode=${2:?Choose historical or current}
contract_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
case "$mode" in
  historical) fixtures="$contract_dir/../src/domain/__fixtures__" ;;
  current) fixtures="$contract_dir/../src/domain/__fixtures__/current" ;;
  *) echo "Unknown contract version: $mode" >&2; exit 2 ;;
esac
scratch=$(mktemp -d "${TMPDIR:-/tmp}/opsloop-dashboard-contract-check.XXXXXXXX")
trap 'rm -rf -- "$scratch"' EXIT
bash "$contract_dir/regenerate.sh" "$backend" "$mode" "$scratch"
for name in go-candidates.json go-quantities.json; do
  cmp -- "$fixtures/$name" "$scratch/$name"
done
printf '%s Go candidate and quantity contracts match\n' "$mode"
