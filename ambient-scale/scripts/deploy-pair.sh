#!/usr/bin/env bash
# Deploy workload pairs for inclusive index range [START .. END].
#   ./deploy-pair.sh 5 9   -> bookinfo-5 .. bookinfo-9 (+ sleep/httpbin/curl)
#   ./deploy-pair.sh 0 4   -> bookinfo-0 .. bookinfo-4
# Safe to re-run: existing indices are reconciled; new indices are added.
set -euo pipefail

START="${1:?Usage: $0 <start-index> <end-index>  e.g. 5 9 for bookinfo-5..bookinfo-9}"
END="${2:?Usage: $0 <start-index> <end-index>  e.g. 5 9 for bookinfo-5..bookinfo-9}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

validate_index() {
  local name="$1" value="$2"
  if ! [[ "${value}" =~ ^[0-9]+$ ]]; then
    echo "Error: ${name} must be a non-negative integer (got: ${value})" >&2
    exit 1
  fi
}

validate_index "start-index" "${START}"
validate_index "end-index" "${END}"

if [[ "${START}" -gt "${END}" ]]; then
  echo "Error: start-index (${START}) must be <= end-index (${END})" >&2
  exit 1
fi

TOTAL=$((END - START + 1))
echo "Deploying ${TOTAL} pairs: indices ${START} .. ${END}"

for i in $(seq "${START}" "${END}"); do
  echo ""
  echo "########## index ${i} (${START}..${END}) ##########"
  "${SCRIPT_DIR}/deploy-index.sh" "${i}"
done

echo ""
echo "Done. Deployed indices ${START}..${END}: bookinfo, sleep, httpbin, curl for each."
