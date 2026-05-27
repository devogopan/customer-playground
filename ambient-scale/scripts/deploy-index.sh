#!/usr/bin/env bash
# Deploy one index: bookinfo-N (ambient) + sleep-N + httpbin-N (sidecar) + curl-N
set -euo pipefail

INDEX="${1:?Usage: $0 <index>  e.g. 3 for bookinfo-3}"
ISTIO_BRANCH="${ISTIO_BRANCH:-release-1.28}"
BASE_URL="https://raw.githubusercontent.com/istio/istio/${ISTIO_BRANCH}/samples"

bookinfo_ns="bookinfo-${INDEX}"
sleep_ns="sleep-${INDEX}"
httpbin_ns="httpbin-${INDEX}"
curl_ns="curl-${INDEX}"

if ! command -v istioctl &>/dev/null; then
  echo "istioctl is required for waypoint enrollment" >&2
  exit 1
fi

echo "=== Ambient bookinfo in ${bookinfo_ns} ==="
kubectl create namespace "${bookinfo_ns}" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "${bookinfo_ns}" istio.io/dataplane-mode=ambient --overwrite

kubectl apply -n "${bookinfo_ns}" -f "${BASE_URL}/bookinfo/platform/kube/bookinfo.yaml"

# Topology: productpage, details, reviews-v2, ratings-v1 (no MongoDB; ratings-v1 is in-memory)
kubectl delete deployment -n "${bookinfo_ns}" \
  reviews-v1 reviews-v3 ratings-v2 mongodb-v1 --ignore-not-found
kubectl delete service -n "${bookinfo_ns}" mongodb --ignore-not-found

kubectl wait -n "${bookinfo_ns}" --for=condition=available deployment/ratings-v1 --timeout=300s
kubectl wait -n "${bookinfo_ns}" --for=condition=available deployment/reviews-v2 --timeout=300s
kubectl wait -n "${bookinfo_ns}" --for=condition=available deployment/productpage-v1 --timeout=300s

echo "Applying namespace waypoint in ${bookinfo_ns}..."
istioctl waypoint apply -n "${bookinfo_ns}" --enroll-namespace --wait

echo "=== Ambient client ${sleep_ns} -> ${bookinfo_ns} ==="
kubectl create namespace "${sleep_ns}" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "${sleep_ns}" istio.io/dataplane-mode=ambient --overwrite

kubectl apply -n "${sleep_ns}" -f "${BASE_URL}/sleep/sleep.yaml"

# Replace idle sleep with steady productpage load
kubectl patch deployment sleep -n "${sleep_ns}" --type=json -p='[
  {"op": "replace", "path": "/spec/template/spec/containers/0/command", "value": ["/bin/sh", "-c"]},
  {"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": [
    "while true; do curl -sf \"http://productpage.'"${bookinfo_ns}"'.svc.cluster.local:9080/productpage\" -o /dev/null || true; sleep 1; done"
  ]}
]'

echo "=== Sidecar httpbin in ${httpbin_ns} ==="
kubectl create namespace "${httpbin_ns}" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "${httpbin_ns}" istio-injection=enabled --overwrite

kubectl apply -n "${httpbin_ns}" -f "${BASE_URL}/httpbin/httpbin.yaml"
kubectl wait -n "${httpbin_ns}" --for=condition=available deployment/httpbin --timeout=300s

echo "=== Sidecar client ${curl_ns} -> ${httpbin_ns} ==="
kubectl create namespace "${curl_ns}" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "${curl_ns}" istio-injection=enabled --overwrite

kubectl apply -n "${curl_ns}" -f "${BASE_URL}/curl/curl.yaml"

kubectl patch deployment curl -n "${curl_ns}" --type=json -p='[
  {"op": "replace", "path": "/spec/template/spec/containers/0/command", "value": ["/bin/sh", "-c"]},
  {"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": [
    "while true; do curl -sf \"http://httpbin.'"${httpbin_ns}"'.svc.cluster.local:8000/get\" -o /dev/null || true; sleep 1; done"
  ]}
]'

echo "Deployed index ${INDEX}: ${bookinfo_ns}, ${sleep_ns}, ${httpbin_ns}, ${curl_ns}"
