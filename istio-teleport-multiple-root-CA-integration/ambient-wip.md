# Istio Ambient + Teleport: Reproducing the Cross-Namespace Failure

This guide builds an **ambient-mode** Istio 1.28 mesh with two waypoint-enabled namespaces:

- `app-citadel`: the waypoint has an **Istio CA** identity (default).
- `app-teleport`: the waypoint has a **Teleport-issued** identity, via tbot and the SPIFFE CSI socket.

It reproduces the failure seen when a call goes through the Teleport-identity waypoint, and explains why it fails. It is the baseline for the experiments listed at the end.

The sidecar-mode setup (which works, using EnvoyFilters) is in the customer playground:
[istio-teleport-multiple-root-CA-integration](https://github.com/devogopan/customer-playground/tree/main/istio-teleport-multiple-root-CA-integration)
([README](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/README.md), [Setup.md](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/Setup.md)).

> **Status:** reproduced on 2026-10-22 with Teleport Cloud 18.11.x (trial), Istio 1.28.6 (ambient), Kubernetes on kind.
>
> | Call | Path | Result |
> |---|---|---|
> | sleep-citadel → httpbin-citadel | ztunnel → **Istio** waypoint → ztunnel | 200 |
> | sleep-teleport → httpbin-citadel | ztunnel → **Istio** waypoint → ztunnel | 200 |
> | sleep-citadel → httpbin-teleport | ztunnel → **Teleport** waypoint | **fails** (connection reset) |
> | sleep-teleport → httpbin-teleport | ztunnel → **Teleport** waypoint | **fails** (connection reset) |

## Table of Contents

- [What changes in ambient mode](#what-changes-in-ambient-mode)
- [Architecture](#architecture)
- [Prerequisites and what is reused](#prerequisites-and-what-is-reused)
- [Step 1 - Create a fresh kind cluster](#step-1---create-a-fresh-kind-cluster)
- [Step 2 - Teleport, tbot, CSI driver and cacerts](#step-2---teleport-tbot-csi-driver-and-cacerts)
- [Step 3 - Install Istio 1.28 in ambient mode](#step-3---install-istio-128-in-ambient-mode)
- [Step 4 - Namespaces and workloads](#step-4---namespaces-and-workloads)
- [Step 5 - Waypoints](#step-5---waypoints)
- [Step 6 - Verify the waypoint identities](#step-6---verify-the-waypoint-identities)
- [Step 7 - Cross-namespace calls](#step-7---cross-namespace-calls)
- [Step 8 - Read the error](#step-8---read-the-error)
- [Experiments to try next](#experiments-to-try-next)
- [Troubleshooting](#troubleshooting)
- [Cleanup](#cleanup)
- [References](#references)

## What changes in ambient mode

| | Sidecar mode (customer playground) | Ambient mode (this guide) |
|---|---|---|
| Who does workload mTLS | `istio-proxy` (Envoy) in each pod | **ztunnel** (Rust, one per node) |
| Where workload identities come from | Teleport (tbot socket) or istiod, per pod | **Always istiod** (ztunnel requests a cert for each pod) |
| L7 proxy | The sidecar | A **waypoint** (Envoy), one per namespace, **destination-side** |
| EnvoyFilter | Supported (break-glass) | **Not supported** on waypoints; ztunnel isn't Envoy |
| External certificate provider (SPIRE / tbot) | Supported via the SDS socket | **Not supported** for workloads ("Ambient mode does not support SPIRE integration", Istio docs) |

What this guide tests: only the **waypoint** in `app-teleport` gets a Teleport identity. The workloads (`httpbin`, `sleep`) in both namespaces still get **Istio CA** identities from ztunnel.

## Architecture

```text
                         Teleport Enterprise Cloud (<tenant>)
                                     ▲
                                     │ tbot (DaemonSet) joins, serves /run/tbot/socket
                                     │
 ┌─────────────────────────── kind cluster (ambient) ───────────────────────────┐
 │                                                                              │
 │  istiod (profile=ambient, trustDomain cluster.local, cacerts = Istio+Teleport roots)
 │                                                                              │
 │  ns app-citadel                         ns app-teleport                      │
 │  ┌───────────────┐                      ┌────────────────────────────────┐   │
 │  │ waypoint      │ Istio CA identity    │ waypoint                       │   │
 │  │ (istio-proxy) │                      │ (istio-proxy + SPIFFE CSI vol) │   │
 │  └───────▲───────┘                      └──────────────▲─────────────────┘   │
 │          │ HBONE                                       │ HBONE               │
 │  httpbin, sleep ◄────────── ztunnel (node) ──────────► httpbin, sleep        │
 │  (Istio CA identities)                                 (Istio CA identities) │
 └──────────────────────────────────────────────────────────────────────────────┘
```

Waypoints are **destination-side**: a call goes through the waypoint of the **service being called**.

- Every call to `httpbin.app-teleport` (from either namespace) goes through the **Teleport** waypoint.
- Every call to `httpbin.app-citadel` goes through the **Istio** waypoint.

## Prerequisites and what is reused

- A Teleport Enterprise Cloud tenant, `tsh`/`tctl` logged in (Setup.md [Part 1](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/Setup.md#part-1---connect-to-teleport-enterprise-cloud)).
- `kind`, `kubectl`, `helm`, `istioctl` 1.28.x, `jq`, `openssl`.
- Shell variables, as in Setup.md:

  ```bash
  export TP_TENANT="<tenant>.teleport.sh"
  export TP_TRUST_DOMAIN="$TP_TENANT"
  export TP_VERSION=$(curl -s "https://$TP_TENANT/webapi/ping" | jq -r '.server_version')
  ```

| Setup.md part | Used here? | Notes |
|---|---|---|
| Part 1 - Connect to Teleport | ✅ | Unchanged |
| Part 2 - Bot, join token, WorkloadIdentity, role | ✅ with **one change** | The bot, `WorkloadIdentity` and role are reused. The **join token is new**: a new cluster has a new service-account signing key (JWKS) |
| Part 3 - tbot DaemonSet | ✅ | Pass the new token name |
| Part 4 - SPIFFE CSI driver | ✅ | Unchanged |
| Part 5 - Istio root CA and `cacerts` | ✅ | Reuse the same `certs/` files; create `cacerts` **before** istiod |
| Part 6 - Two sidecar revisions | ❌ | Replaced by the single ambient istiod (Step 3) |
| Part 7 - `spiffe_federation` | Optional | Not needed to reproduce the failure. Keep it if you plan the waypoint-shim experiment |
| Parts 8-9 - Sidecar workloads, EnvoyFilters | ❌ | EnvoyFilters don't apply in ambient |

## Step 1 - Create a fresh kind cluster

A separate cluster keeps the ambient components (ambient istiod, `istio-cni` in ambient mode, ztunnel) apart from the sidecar setup.

```bash
kind create cluster --name ambient-1.28
kubectl config use-context kind-ambient-1.28
```

## Step 2 - Teleport, tbot, CSI driver and cacerts

### 2.1 New join token for this cluster

Same as Setup.md [Part 2.2](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/Setup.md#22-kubernetes-join-token-static-jwks), with a new token name:

```bash
kubectl get --raw /openid/v1/jwks > jwks-ambient.json
jq -e '.keys[0].n' jwks-ambient.json >/dev/null && echo "JWKS OK"

cat > bot-token-ambient.yaml <<EOF
kind: token
version: v2
metadata:
  name: istio-ambient-bot
spec:
  roles: [Bot]
  bot_name: istio-local
  join_method: kubernetes
  kubernetes:
    type: static_jwks
    static_jwks:
      jwks: '$(jq -c . jwks-ambient.json)'
    allow:
      - service_account: "default:tbot"
EOF

tctl create -f bot-token-ambient.yaml --force
```

### 2.2 tbot DaemonSet and SPIFFE CSI driver

Setup.md [Part 3](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/Setup.md#part-3---deploy-tbot-daemonset) and [Part 4](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/Setup.md#part-4---install-spiffe-csi-driver):

```bash
helm upgrade --install tbot-wi ./tbot-wi-ds -n default \
  --set clusterName="$TP_TRUST_DOMAIN" \
  --set teleportProxyAddress="$TP_TENANT:443" \
  --set token=istio-ambient-bot \
  --set image.tag="$TP_VERSION"
kubectl -n default rollout status ds/tbot-wi
kubectl -n default logs -l app=tbot-wi --tail=20 | grep -E "Fetched new bot identity|Listener opened"

kubectl create namespace spiffe-csi
kubectl apply -f spiffe-csi-driver.yaml
kubectl -n spiffe-csi rollout status ds/spiffe-csi-driver
```

### 2.3 `cacerts` with the Istio root and the Teleport root

Setup.md [Part 5.3](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/Setup.md#53-build-the-combined-root-and-create-cacerts). Reuse the existing `certs/` directory:

```bash
kubectl create namespace istio-system
kubectl -n istio-system create secret generic cacerts \
  --from-file=certs/rootA/intermediateA/ca-cert.pem \
  --from-file=certs/rootA/intermediateA/ca-key.pem \
  --from-file=root-cert.pem=certs/combined-root.pem \
  --from-file=certs/rootA/intermediateA/cert-chain.pem
```

If the tenant changed since `certs/combined-root.pem` was built, re-run Setup.md Part 5.2 to fetch the new Teleport root first.

## Step 3 - Install Istio 1.28 in ambient mode

From the Istio 1.28 [ambient Helm install](https://istio.io/v1.28/docs/ambient/install/helm/). Gateway API **v1.4.0** is the version pinned for Istio 1.28.

```bash
helm repo add istio https://istio-release.storage.googleapis.com/charts
helm repo update

kubectl get crd gateways.gateway.networking.k8s.io &>/dev/null || \
  kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/experimental-install.yaml

helm install istio-base istio/base -n istio-system --version 1.28.6 --wait

helm install istiod istio/istiod -n istio-system --version 1.28.6 \
  --set profile=ambient \
  --set meshConfig.trustDomain=cluster.local \
  --set "meshConfig.trustDomainAliases[0]=$TP_TRUST_DOMAIN" \
  --set meshConfig.accessLogFile=/dev/stdout \
  --wait

helm install istio-cni istio/cni -n istio-system --version 1.28.6 --set profile=ambient --wait
helm install ztunnel   istio/ztunnel -n istio-system --version 1.28.6 --wait
```

`trustDomainAliases` affects only the waypoints (Envoy). ztunnel has no alias support.

```bash
kubectl -n istio-system get pods
# istiod, istio-cni-node, ztunnel all Running
```

## Step 4 - Namespaces and workloads

Both namespaces run in ambient mode. The workloads have **no sidecar annotations and no CSI volume**; ztunnel handles their mTLS.

```bash
for NS in app-citadel app-teleport; do
  kubectl create namespace $NS
  kubectl label namespace $NS istio.io/dataplane-mode=ambient
done
```

`workloads-ambient.yaml`: the same `httpbin` and `sleep` as the playground's [`httpbin-citadel-basic.yaml`](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/httpbin-citadel-basic.yaml) / [`sleep-citadel-basic.yaml`](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/sleep-citadel-basic.yaml), without the sidecar annotations:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: httpbin
---
apiVersion: v1
kind: Service
metadata:
  name: httpbin
  labels: { app: httpbin, service: httpbin }
spec:
  ports:
    - name: http
      port: 8000
      targetPort: 80
  selector:
    app: httpbin
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: httpbin
spec:
  replicas: 1
  selector:
    matchLabels: { app: httpbin }
  template:
    metadata:
      labels: { app: httpbin }
    spec:
      serviceAccountName: httpbin
      containers:
        - name: httpbin
          image: kennethreitz/httpbin
          ports:
            - containerPort: 80
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: sleep
---
apiVersion: v1
kind: Service
metadata:
  name: sleep
  labels: { app: sleep, service: sleep }
spec:
  ports:
    - name: http
      port: 80
  selector:
    app: sleep
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sleep
spec:
  replicas: 1
  selector:
    matchLabels: { app: sleep }
  template:
    metadata:
      labels: { app: sleep }
    spec:
      terminationGracePeriodSeconds: 0
      serviceAccountName: sleep
      containers:
        - name: sleep
          image: docker.io/curlimages/curl:8.16.0
          command: ["/bin/sleep", "infinity"]
```

```bash
kubectl -n app-citadel  apply -f workloads-ambient.yaml
kubectl -n app-teleport apply -f workloads-ambient.yaml
for NS in app-citadel app-teleport; do
  for D in httpbin sleep; do kubectl -n $NS rollout status deploy/$D; done
  kubectl -n $NS get pods          # READY 1/1: no sidecars in ambient
done
```

## Step 5 - Waypoints

### 5.1 `app-citadel`: Istio CA identity (default)

`waypoint-app-citadel.yaml`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: waypoint
  namespace: app-citadel
  labels:
    istio.io/waypoint-for: service
spec:
  gatewayClassName: istio-waypoint
  listeners:
    - name: mesh
      port: 15008
      protocol: HBONE
```

### 5.2 `app-teleport`: Teleport identity

The Istio 1.28 waypoint template already mounts an `emptyDir` called `workload-socket` at `/var/run/secrets/workload-spiffe-uds` in `istio-proxy`. The Gateway's `spec.infrastructure.parametersRef` points at a ConfigMap whose `deployment` key is applied to the generated Deployment as a strategic merge patch ([Istio docs: Gateway API, automated deployment customization](https://istio.io/v1.28/docs/tasks/traffic-management/ingress/gateway-api/#automated-deployment)). The patch replaces that `emptyDir` with the SPIFFE CSI socket. istio-agent in the waypoint then detects the socket and gets its certificate from tbot.

`waypoint-app-teleport.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: waypoint-teleport-identity
  namespace: app-teleport
data:
  deployment: |
    spec:
      template:
        spec:
          volumes:
            - name: workload-socket
              emptyDir: null          # remove the template's emptyDir; a volume can have only one source
              csi:
                driver: csi.spiffe.io
                readOnly: true
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: waypoint
  namespace: app-teleport
  labels:
    istio.io/waypoint-for: service
spec:
  gatewayClassName: istio-waypoint
  infrastructure:
    parametersRef:
      group: ""
      kind: ConfigMap
      name: waypoint-teleport-identity
  listeners:
    - name: mesh
      port: 15008
      protocol: HBONE
```

- Without `emptyDir: null`, the merge keeps both `emptyDir` and `csi`, and Kubernetes rejects the volume.
- The waypoint's service account is `waypoint`. The existing `WorkloadIdentity` template (`/ns/{{ workload.kubernetes.namespace }}/sa/{{ workload.kubernetes.service_account }}`) issues `spiffe://<tenant>/ns/app-teleport/sa/waypoint`.

### 5.3 Apply and enroll the namespaces

```bash
kubectl apply -f waypoint-app-citadel.yaml -f waypoint-app-teleport.yaml
kubectl label namespace app-citadel  istio.io/use-waypoint=waypoint
kubectl label namespace app-teleport istio.io/use-waypoint=waypoint

kubectl -n app-citadel  rollout status deploy/waypoint
kubectl -n app-teleport rollout status deploy/waypoint
```

## Step 6 - Verify the waypoint identities

```bash
# The Teleport waypoint's volume must be CSI, not emptyDir
kubectl -n app-teleport get deploy waypoint -o json \
  | jq '.spec.template.spec.volumes[] | select(.name=="workload-socket")'
# { "csi": { "driver": "csi.spiffe.io", "readOnly": true }, "name": "workload-socket" }

# istio-agent detected the external socket
WP=$(kubectl -n app-teleport get pod -l gateway.networking.k8s.io/gateway-name=waypoint -o jsonpath='{.items[0].metadata.name}')
kubectl -n app-teleport logs $WP | grep -E "workload SDS socket|Skipping connecting to CA|generated new workload certificate"
# Existing workload SDS socket found at var/run/secrets/workload-spiffe-uds/socket. ...
# Workload is using file mounted certificates. Skipping connecting to CA

# Who issued each waypoint's certificate (by issuer, not just SAN)
for NS in app-citadel app-teleport; do
  WP=$(kubectl -n $NS get pod -l gateway.networking.k8s.io/gateway-name=waypoint -o jsonpath='{.items[0].metadata.name}')
  echo "== $NS"
  istioctl pc s $WP.$NS -o json \
    | jq -r '.dynamicActiveSecrets[] | select(.name=="default") | .secret.tlsCertificate.certificateChain.inlineBytes' \
    | base64 -d | openssl x509 -noout -issuer -ext subjectAltName -dates
done
```

Expected (as observed):

| Waypoint | Issuer | SAN | Lifetime |
|---|---|---|---|
| `app-citadel` | `O=Istio, CN=Intermediate CA, L=intermediateA` | `spiffe://cluster.local/ns/app-citadel/sa/waypoint` | ~24h |
| `app-teleport` | `O=<tenant>, CN=<tenant>` | `spiffe://<tenant>/ns/app-teleport/sa/waypoint` | ~1h |

```bash
istioctl pc s $WP.app-teleport
# default  Cert Chain  ACTIVE ... TRUST DOMAIN <tenant>
# ROOTCA   CA          ACTIVE ... TRUST DOMAIN <tenant>     <- Teleport trust domain only
```

## Step 7 - Cross-namespace calls

```bash
call() { kubectl -n "$1" exec deploy/sleep -- curl -s -o /dev/null -w "%{http_code}\n" "http://httpbin.$2:8000/headers"; }
call app-citadel  app-citadel     # via Istio waypoint
call app-teleport app-citadel     # via Istio waypoint
call app-citadel  app-teleport    # via Teleport waypoint
call app-teleport app-teleport    # via Teleport waypoint
```

Observed:

```text
200
200
000
command terminated with exit code 56
000
command terminated with exit code 56
```

`000` with curl exit code 56 is a connection reset: the TLS handshake to the waypoint failed before any HTTP response.

```bash
istioctl ztunnel-config workload | grep -E "NAME|waypoint"
```

## Step 8 - Read the error

```bash
kubectl -n istio-system logs ds/ztunnel --since=5m | grep -E "error.*access" | tail -5
```

Observed (tenant name replaced):

```text
error access connection complete
  src.workload="sleep-..." src.namespace="app-citadel" src.identity="spiffe://cluster.local/ns/app-citadel/sa/sleep"
  dst.addr=10.244.0.28:15008 dst.service="httpbin.app-teleport.svc.cluster.local"
  dst.workload="waypoint-..." dst.namespace="app-teleport"
  dst.identity="spiffe://cluster.local/ns/app-teleport/sa/waypoint"
  direction="outbound" bytes_sent=0 bytes_recv=0 duration="0ms"
  error="io error: invalid peer certificate: Other(OtherError(identity verification error:
         peer did not present the expected SAN (spiffe://cluster.local/ns/app-teleport/sa/waypoint),
         got spiffe://<tenant>/ns/app-teleport/sa/waypoint))"
```

Calls to `httpbin.app-citadel` log `info access connection complete` through the Istio waypoint, with `dst.identity="spiffe://cluster.local/ns/app-citadel/sa/waypoint"`.

### Why it fails

A call through the Teleport waypoint has three TLS hops. Each is checked by a different component:

| Hop | Component | Check | Source (1.28) | Result here |
|---|---|---|---|---|
| ① caller's ztunnel → Teleport waypoint | **ztunnel** | Chain must verify against ztunnel's roots, **then** the SPIFFE ID must **exactly** match the identity istiod assigns | ztunnel `src/tls/workload.rs` `verify_server_cert` (chain check, then `verify_full_san`); error text in `src/tls/lib.rs`. The expected trust domain comes from `pickTrustDomain(meshCfg)` in istiod `pilot/pkg/serviceregistry/kube/controller/ambient/workloads.go` (mesh-wide; no per-workload override) | ❌ **Fails here.** Chain OK, SAN mismatch: expects `spiffe://cluster.local/…`, gets `spiffe://<tenant>/…` |
| ② Teleport waypoint → destination ztunnel | **ztunnel** | The client cert's trust domain must **equal** the trust domain of the destination workload's own cert | ztunnel `TrustDomainVerifier` in `src/tls/workload.rs`; `td` from `self.cert.identity()` in `src/tls/certificate.rs` | Not reached. Would fail: `<tenant>` ≠ `cluster.local` |
| ③ inside the Teleport waypoint | **Envoy** | Peer certs are checked against `ROOTCA` from tbot | tbot `lib/tbot/internal/sds/handler.go`: `ROOTCA` = own trust domain only | Not reached. Would fail: Istio-signed peers aren't in the Teleport-only bundle |

Key findings:

1. **ztunnel trusts the Teleport root.** The error is a SAN error, and ztunnel checks the chain *before* the SAN, so the chain check passed. The Teleport root in `cacerts` reached ztunnel's trust store.
2. **The blocker is identity, not trust.** ztunnel requires the waypoint's SPIFFE ID in the mesh trust domain (`cluster.local`).
3. **No alias support in ztunnel.** `meshConfig.trustDomainAliases` affects Envoy only. ztunnel's `src/config.rs` has only a TODO mentioning `trustDomainAliases`.
4. **EnvoyFilter can't help** with hops ① and ②, which are in ztunnel. It's also unsupported on waypoints (hop ③): "*`EnvoyFilter` is not currently supported for any existing Istio version with waypoint proxies*" (Istio docs, ambient L7 features).
5. **Workloads are Istio-issued regardless.** Even if this worked, only the waypoint hop would carry a Teleport identity.

## Experiments to try next

Each experiment changes one thing against this baseline. The prediction is written down first, so the result can be compared with it.

| # | Change | Prediction | Why |
|---|---|---|---|
| E1 | Set the **mesh trust domain to Teleport's**: `helm upgrade istiod … --set meshConfig.trustDomain=$TP_TRUST_DOMAIN`, then restart ztunnel, waypoints and workloads | ① and ② **pass**; ③ **fails** (`CERTIFICATE_VERIFY_FAILED` in the Teleport waypoint) | The SAN and trust domain would match. But the waypoint's `ROOTCA` binds `<tenant>` to the Teleport root only, and Istio-signed certs would now claim `<tenant>` too |
| E2 | E1 plus a **modified SDS shim** in the Teleport waypoint that **adds the Istio root to the `<tenant>` trust bundle** of `ROOTCA` | Might work end to end | Fixes ③. But two CAs issue for one trust domain (the Istio CA could mint any `spiffe://<tenant>/…` ID), and workloads are still Istio-issued. A lab curiosity, not a recommendation |
| E3 | **Istio as registration authority** (`EXTERNAL_CA=ISTIOD_RA_KUBERNETES_API`) with a **Teleport-backed Kubernetes CSR signer** (custom controller calling Teleport `IssueWorkloadIdentity` with the CSR's public key and the pod's namespace/service account) | Could give **all** ambient workloads and waypoints Teleport-issued identities, with no shim and no EnvoyFilter | Istio's RA mode is *Experimental*; ztunnel sends no per-workload signer, so it's one signer for the whole mesh; the mesh trust domain must equal Teleport's; the controller is new, security-critical custom code; no off-the-shelf implementation exists |

For each experiment, capture the same evidence as Steps 6–8: waypoint issuer and SAN, the four calls, the ztunnel error lines, and the Teleport waypoint's Envoy log:

```bash
kubectl -n app-teleport logs $WP | grep -iE "TLS|CERTIFICATE| 503 " | tail -20
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Teleport waypoint issuer is `O=Istio` | The ConfigMap wasn't applied to the generated Deployment | Check `kubectl -n app-teleport get gateway waypoint -o yaml` status conditions; the ConfigMap must be in the same namespace as the Gateway |
| Waypoint Deployment rejected: volume has more than one source | `emptyDir: null` missing from the ConfigMap patch | Add it (Step 5.2) |
| Waypoint log has no `Existing workload SDS socket found` | CSI driver not running on that node, or tbot socket missing | `kubectl -n spiffe-csi get pods -o wide`; `kubectl -n default logs -l app=tbot-wi` |
| Waypoint stuck without a certificate | tbot can't attest the waypoint pod, or no matching `WorkloadIdentity` | tbot logs; Setup.md Parts 2.3–2.4 |
| tbot join fails with a JWKS error | Old cluster's JWKS in the token | Redo Step 2.1 against this cluster |
| All calls fail, including through the Istio waypoint | Ambient not active: missing namespace label, or `istio-cni`/ztunnel not ready | `kubectl get ns -L istio.io/dataplane-mode,istio.io/use-waypoint`; `kubectl -n istio-system get pods` |
| ztunnel error `UnknownIssuer` instead of the SAN error | The Teleport root isn't in `cacerts` | Rebuild `combined-root.pem` (Setup.md Part 5.2–5.3), recreate `cacerts`, restart istiod and ztunnel |

## Cleanup

```bash
kind delete cluster --name ambient-1.28
tctl rm token/istio-ambient-bot
```

## References

- Customer playground, sidecar mode: [istio-teleport-multiple-root-CA-integration](https://github.com/devogopan/customer-playground/tree/main/istio-teleport-multiple-root-CA-integration): [README](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/README.md), [Setup.md](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/Setup.md), [SDS-shim.md](https://github.com/devogopan/customer-playground/blob/main/istio-teleport-multiple-root-CA-integration/SDS-shim.md)
- Istio 1.28 ambient Helm install: https://istio.io/v1.28/docs/ambient/install/helm/
- Istio waypoints: https://istio.io/v1.28/docs/ambient/usage/waypoint/
- Istio Gateway deployment customization (`parametersRef`): https://istio.io/v1.28/docs/tasks/traffic-management/ingress/gateway-api/#automated-deployment
- Istio ambient migration blockers ("Ambient mode does not support SPIRE integration"): https://istio.io/latest/docs/ambient/migrate/
- Istio ambient L7 features (EnvoyFilter not supported on waypoints): https://istio.io/latest/docs/ambient/usage/l7-features/
- Istio custom CA via Kubernetes CSR (RA mode, Experimental): https://istio.io/v1.28/docs/tasks/security/cert-management/custom-ca-k8s/
- ztunnel source (release-1.28): https://github.com/istio/ztunnel/tree/release-1.28/src/tls
- istiod ambient workload trust domain (`pickTrustDomain`): https://github.com/istio/istio/blob/release-1.28/pilot/pkg/serviceregistry/kube/controller/ambient/workloads.go
- Teleport tbot SDS names (`ROOTCA` / `ALL`): https://github.com/gravitational/teleport/blob/branch/v18/lib/tbot/internal/sds/handler.go
- Teleport workload identity issuance API (`IssueWorkloadIdentity`): https://github.com/gravitational/teleport/blob/branch/v18/api/proto/teleport/workloadidentity/v1/issuance_service.proto
