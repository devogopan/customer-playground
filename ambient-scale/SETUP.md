# Ambient + sidecar hybrid scale testbed

Scale and resource comparison for **Istio ambient** (`ztunnel`, **waypoint**) vs **sidecar** on a small **3-node GKE** cluster.

Target Istio version: **1.28.7** (Helm: `base`, `istiod`, `cni`, `ztunnel` — **no ingress gateway**).

Workloads (scalable 0–9, default examples use 0–2):

| Index | Ambient (Bookinfo) | Ambient client | Sidecar (httpbin) | Sidecar client |
|-------|-------------------|----------------|---------------|----------------|
| N | namespace `bookinfo-N` | namespace `sleep-N` | namespace `httpbin-N` | namespace `curl-N` |

---

## Prerequisites

- `gcloud`, `kubectl`, `helm` (≥ 3.6)
- `istioctl` 1.28.7 (for waypoint enrollment and checks)
- GCP project with billing enabled

Optional: set defaults once:

```bash
export PROJECT_ID="your-gcp-project"
export REGION="us-central1"
export ZONE="us-central1-a"
export CLUSTER_NAME="ambient-scale"
export ISTIO_VERSION="1.28.7"
```

---

## 1. Create a 3-node GKE cluster (same zone)

Cost-conscious defaults: one zone, modest machine type, no autoscaling above 3.

```bash
gcloud container clusters create "${CLUSTER_NAME}" \
  --project="${PROJECT_ID}" \
  --zone="${ZONE}" \
  --num-nodes=3 \
  --machine-type=e2-standard-4 \
  --disk-type=pd-standard \
  --disk-size=50 \
  --no-enable-autoupgrade \
  --no-enable-autorepair

gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --project="${PROJECT_ID}" \
  --zone="${ZONE}"
```

Verify nodes:

```bash
kubectl get nodes -o wide
```

**Correction notes (fill in after your run):**

- Machine type / disk if CPU or memory saturated: _______________
- Whether preemptible nodes are acceptable: _______________

---

## 2. Install Istio 1.28.7 (ambient, Helm, no gateway)

First-time install only (`helm install`, not upgrade). Official guide: [Ambient install with Helm (v1.28)](https://istio.io/v1.28/docs/ambient/install/helm/)

### manual install

#### Step 0 — GKE only (required before first `istio-cni` install)

**This step is required on GKE.** Skip it on other platforms (EKS, kind, minikube, etc.).

On GKE, `istio-cni` and `ztunnel` use the `system-node-critical` PriorityClass. GKE only allows that class in namespaces that have a matching **ResourceQuota** — by default only `kube-system` has one. Without the quota below, the CNI DaemonSet fails on first install with:

```text
Error creating: insufficient quota to match these scopes:
[{PriorityClass In [system-node-critical system-cluster-critical]}]
```

Create `istio-system` (if not already present) and apply the quota **before** installing `istio-cni` or `ztunnel`:

```bash
kubectl create namespace istio-system 
kubectl apply -f manifests/gke/resourcequota-critical-pods.yaml
```

Reference: [Istio GKE platform prerequisites](https://istio.io/v1.28/docs/ambient/install/platform-prerequisites/#google-kubernetes-engine-gke)

#### Steps 1–2 — all platforms

```bash
helm repo add istio https://istio-release.storage.googleapis.com/charts
helm repo update

kubectl get crd gateways.gateway.networking.k8s.io &>/dev/null || \
  kubectl apply --server-side -f \
    https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/experimental-install.yaml

helm install istio-base istio/base -n istio-system --create-namespace \
  --version "${ISTIO_VERSION}" --wait

helm install istiod istio/istiod -n istio-system \
  --version "${ISTIO_VERSION}" \
  --set profile=ambient --wait
```

#### Steps 3–4 — CNI and ztunnel (pick your platform)

**On GKE** — run Step 0 first, then:

```bash
helm install istio-cni istio/cni -n istio-system \
  --version "${ISTIO_VERSION}" \
  --set profile=ambient \
  --set global.platform=gke \
  --wait

helm install ztunnel istio/ztunnel -n istio-system \
  --version "${ISTIO_VERSION}" \
  --wait
```

**Not on GKE** — no ResourceQuota or `global.platform` needed:

```bash
helm install istio-cni istio/cni -n istio-system \
  --version "${ISTIO_VERSION}" \
  --set profile=ambient --wait

helm install ztunnel istio/ztunnel -n istio-system \
  --version "${ISTIO_VERSION}" --wait
```

Verify:

```bash
helm ls -n istio-system
kubectl get pods -n istio-system
kubectl get daemonset -n istio-system
```

Expected components: `istiod`, `istio-cni-node`, `ztunnel` (DaemonSet on each node).

**No** `istio/gateway` chart is installed.

---

## 3. Deploy scaled workload pairs

### 3.1 Index range (inclusive)

```bash
./scripts/deploy-pair.sh 0 4    # bookinfo-0 .. bookinfo-4 (+ sleep/httpbin/curl)
./scripts/deploy-pair.sh 5 9    # bookinfo-5 .. bookinfo-9 only
```

Re-running is safe: if `0-4` already exist, `./scripts/deploy-pair.sh 0 9` reconciles `0-4` and adds `5-9`.

### 3.2 Deploy `0` .. `N-1` 

```bash
./scripts/deploy-scale.sh 10    # same as ./scripts/deploy-pair.sh 0 9
```

### What each pair does

**Ambient — `bookinfo-N`**

- Namespace label: `istio.io/dataplane-mode=ambient`
- Apps from Istio `release-1.28` samples:
  - `productpage`, `details`, `reviews-v2`, `ratings-v1` (no MongoDB; ratings are in-memory)
- Namespace waypoint (for L7 path / waypoint CPU measurement):

  ```bash
  istioctl waypoint apply -n "bookinfo-${N}" --enroll-namespace --wait
  ```

**Ambient client — `sleep-N`**

- Also `istio.io/dataplane-mode=ambient`
- Pod runs a loop: `GET http://productpage.bookinfo-N.svc.cluster.local:9080/productpage`
- Traffic hits productpage → details / reviews / ratings in that namespace

**Sidecar — `httpbin-N`**

- Namespace label: `istio-injection=enabled`
- Istio sample `httpbin`

**Sidecar client — `curl-N`**

- Injected curl pod; loop: `GET http://httpbin.httpbin-N.svc.cluster.local:8000/get`

---

## 4. Verify traffic

```bash
# Bookinfo chain (from sleep-0)
kubectl exec -n sleep-0 deploy/sleep -- \
  curl -sf "http://productpage.bookinfo-0.svc.cluster.local:9080/productpage" | head -c 200

# httpbin (from curl-0)
kubectl exec -n curl-0 deploy/curl -- \
  curl -sf "http://httpbin.httpbin-0.svc.cluster.local:8000/get" -o /dev/null -w "%{http_code}\n"
```

Check ambient enrollment:

```bash
kubectl get ns -L istio.io/dataplane-mode,istio.io/use-waypoint
kubectl get pods -n bookinfo-0
kubectl get gateway -n bookinfo-0 2>/dev/null || true
kubectl get pods -n bookinfo-0 -l gateway.networking.k8s.io/gateway-name=waypoint
```

---



```bash
# Workloads only
for i in $(seq 0 9); do
  kubectl delete ns "bookinfo-${i}" "sleep-${i}" "httpbin-${i}" "curl-${i}" --wait=false
done

# Istio
helm delete ztunnel istio-cni istiod istio-base -n istio-system
kubectl delete ns istio-system

# Cluster
gcloud container clusters delete "${CLUSTER_NAME}" --zone="${ZONE}" --quiet
```

---

