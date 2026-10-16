# SDS Shim: Federated Trust for Istio + tbot Without EnvoyFilters
 
## Table of Contents

- [Why a shim](#why-a-shim)
- [What it does and does not do](#what-it-does-and-does-not-do)
- [Prerequisites](#prerequisites)
- [Step 1 - Test and build the image](#step-1---test-and-build-the-image)
- [Step 2 - Make the image available to the cluster](#step-2---make-the-image-available-to-the-cluster)
- [Step 3 - Remove the Part 9 EnvoyFilters](#step-3---remove-the-part-9-envoyfilters)
- [Step 4 - Add the shim to app-teleport workloads](#step-4---add-the-shim-to-app-teleport-workloads)
- [Step 5 - Verify](#step-5---verify)
- [Optional - Inject the shim automatically](#optional---inject-the-shim-automatically)
- [Security and isolation](#security-and-isolation)
- [Resource footprint](#resource-footprint)
- [Troubleshooting](#troubleshooting)
- [Caveats](#caveats)

## Why a shim

Istio's Envoy always requests the trust bundle under the SDS name `ROOTCA`. It is a hard-coded constant in istiod (`SDSRootResourceName`), with no setting to change it. tbot serves:

| SDS name | tbot returns |
|---|---|
| `ROOTCA` | Own trust domain only |
| `ALL` | Own **plus federated** trust domains (as a SPIFFE cert validator) |

SPIRE solves this on the server side (`default_all_bundles_name = "ROOTCA"`). tbot has no equivalent yet, so the shim does it in front of tbot:

```text
istio-proxy (Envoy) ──"ROOTCA"──▶ sds-shim ──"ALL"──▶ tbot
istio-proxy (Envoy) ◀─"ROOTCA" (contents of ALL)── sds-shim ◀─"ALL"── tbot
```

Compared with the EnvoyFilters:

| | Part 9 EnvoyFilters | SDS shim |
|---|---|---|
| New inbound port | Patch or re-verify | Nothing |
| New Citadel destination service | New outbound patch | Nothing |
| TCP catch-all chain | Unreliable | Covered |
| Depends on | Istio-generated chain names, ALPN lists, merge code | Envoy's public SDS API |
| Per-pod cost | None | One small container |

## What it does and does not do

[`sds-shim/main.go`](sds-shim/main.go) is about 150 lines:

- Listens on **one Unix socket** (`/var/run/secrets/workload-spiffe-uds/socket`, where istio-agent looks for an external SDS server).
- Connects to **one Unix socket**: tbot's, via the SPIFFE CSI volume.
- Implements the SDS `StreamSecrets` and `FetchSecrets` RPCs:
  - Requests: rewrites the name `ROOTCA` → `ALL`; everything else unchanged.
  - Responses: renames the `ALL` secret → `ROOTCA`; everything else (the `default` SVID, versions, nonces) unchanged.
- Refuses to start if another process is already serving its socket path (istio-agent started first). The container then crash-loops visibly instead of the pod silently running on istiod certs. A stale socket file with no listener is replaced.
- Implements nothing else: no SPIFFE Workload API, no delta SDS, no TCP listener, no metrics, no Kubernetes API calls.

It must run **inside each workload pod**. tbot attests the process that connects to its socket (peer credentials → PID → pod). A container in the same pod resolves to the same pod, namespace and service account. A shared shim (per node, or inside the tbot pod) would make every workload get the shim's identity.

## Prerequisites

- **Kubernetes 1.34+** for native sidecars (`initContainers` with `restartPolicy: Always`):
- Setup.md Parts 1–8 done: tbot joined, CSI driver running, `cacerts` with combined roots, both istiods with `trustDomainAliases`, `spiffe_federation` synced (Part 7.3 shows the Istio root as a federated bundle).


  ```bash
  kubectl version | grep -i server
  ```

- On the build host: Docker. Go 1.25+ only if you want to run the tests outside Docker.

## Step 1 - Test and build the image

```text
sds-shim/
├── main.go         # the shim
├── main_test.go    # end-to-end test: fake tbot <- shim <- fake Envoy, over Unix sockets
├── go.mod / go.sum
├── Dockerfile      # static binary on scratch, runs as UID 65532
└── .dockerignore
```

```bash
cd sds-shim

# Optional: run the test (needs Go 1.25+)
go test -v ./...
# --- PASS: TestShim
#     tbot was asked for: [ALL default ALL]     <- never ROOTCA

# Build (use --platform to match your nodes; EC2/kind on x86 = linux/amd64)
docker build --platform linux/amd64 -t sds-shim:0.1.0 .
docker images sds-shim:0.1.0      # ~14 MB
```

## Step 2 - Make the image available to the cluster

**kind:**

```bash
kind get clusters                            # e.g. istio-1.28
kind load docker-image sds-shim:0.1.0 --name <kind-cluster>
```

**Registry** (any other cluster):

```bash
docker tag sds-shim:0.1.0 <registry>/sds-shim:0.1.0
docker push <registry>/sds-shim:0.1.0
# then use <registry>/sds-shim:0.1.0 below, ideally pinned by digest (@sha256:...)
```

## Step 3 - Remove the Part 9 EnvoyFilters in Setup.md

Leave them in place and Envoy would request both `ROOTCA` and `ALL`, which the shim can't tell apart in responses. Remove them first:

```bash
kubectl -n app-teleport delete envoyfilter teleport-inbound teleport-outbound --ignore-not-found
```

## Step 4 - Add the shim to app-teleport workloads

### 4.1 Make `istio-proxy` start after the shim

The shim's socket must be listening **before** `istio-proxy` starts. If istio-agent finds no socket at startup, it starts its own SDS server on that path and fetches certs from istiod. The pod then runs on **Istio-signed certs that still carry a Teleport SPIFFE ID**, and nothing fails.

**Istio can't be told to start the shim first.** `ENABLE_NATIVE_SIDECARS` defaults to `auto` in Istio 1.28, which turns native mode on for nodes that support it (Kubernetes 1.29+). In native mode, the injector's `reorderPod` (`pkg/kube/inject/webhook.go`) **always** moves `istio-proxy` to the front of `initContainers`:

```text
istio-init / istio-validation  →  istio-proxy  →  everything else (including sds-shim)
```

`reorderPod` runs unconditionally, after all injection templates are merged, and has no setting to change the position. Kubernetes starts native sidecars strictly in list order, so `istio-proxy` would **always** start before `sds-shim`.

The fix is to inject `istio-proxy` as a **regular container** for the Teleport revision. Kubernetes guarantees that every native sidecar (`sds-shim`) passes its startup probe before **any** regular container starts.

**Revision-wide (recommended).** This affects only `istiod-1-28-6`; the Citadel revision is untouched:

```bash
helm upgrade istiod-1-28-6 istio/istiod -n istio-system --version 1.28.6 \
  --reuse-values \
  --set pilot.env.ENABLE_NATIVE_SIDECARS=false
```

**Per pod (safety net).** Also add this annotation to each pod template (4.2). It takes precedence over `ENABLE_NATIVE_SIDECARS`, so a pod stays correct even if the revision setting is changed later:

```yaml
sidecar.istio.io/nativeSidecar: "false"
```

What `app-teleport` gives up by running `istio-proxy` as a regular container:

| Native-sidecar benefit | Impact |
|---|---|
| App **init containers** can use the mesh (proxy already running) | Only matters if app init containers make mTLS calls |
| **Jobs/CronJobs** finish cleanly (the proxy stops with the job) | Jobs need the classic `pilot-agent request POST quitquitquit` workaround |
| Proxy stops **after** the app at shutdown | Uses `terminationDrainDuration` and the preStop drain instead; the pre-native default, fine for most services |
| App waits for a ready proxy | Still available with `holdApplicationUntilProxyStarts: true` |

When tbot can serve federated bundles as `ROOTCA` (removing the need for the shim), set `ENABLE_NATIVE_SIDECARS` back to `auto` and remove the annotation.

### 4.2 Update the pod templates

Four changes to each `app-teleport` pod template, compared with README-1 Part 8.1:

1. **Add `sds-shim` as a native sidecar** (`initContainers` + `restartPolicy: Always`). Its startup probe holds back the regular containers, including `istio-proxy`, until the shim's socket is listening.
2. **Annotate `sidecar.istio.io/nativeSidecar: "false"`** (the safety net from 4.1).
3. **`workload-socket` becomes a memory `emptyDir`** holding the shim's socket. Istio's injector mounts this volume into `istio-proxy`.
4. **The SPIFFE CSI volume moves to `tbot-upstream`**, mounted **only** in the shim.

`httpbin-teleport-shim.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: httpbin
  namespace: app-teleport
spec:
  replicas: 1
  selector:
    matchLabels:
      app: httpbin
  template:
    metadata:
      labels:
        app: httpbin
      annotations:
        inject.istio.io/templates: "sidecar"
        sidecar.istio.io/nativeSidecar: "false"    # istio-proxy must start after sds-shim
    spec:
      serviceAccountName: httpbin
      initContainers:
        - name: sds-shim
          image: sds-shim:0.1.0
          imagePullPolicy: IfNotPresent
          restartPolicy: Always                    # native sidecar: runs for the pod's lifetime
          args:
            - -listen=/var/run/secrets/workload-spiffe-uds/socket
            - -upstream=/run/tbot-upstream/socket
          startupProbe:                            # istio-proxy starts only after this passes
            exec:
              command: ["/sds-shim", "-check=/var/run/secrets/workload-spiffe-uds/socket"]
            periodSeconds: 1
            failureThreshold: 30
          env:
            - name: GOMEMLIMIT                     # keep the Go GC well under the memory limit
              value: "24MiB"
          resources:
            requests: { cpu: 5m, memory: 16Mi }
            limits:   { memory: 32Mi }
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            runAsGroup: 65532
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
            seccompProfile: { type: RuntimeDefault }
          volumeMounts:
            - name: workload-socket
              mountPath: /var/run/secrets/workload-spiffe-uds
            - name: tbot-upstream
              mountPath: /run/tbot-upstream
              readOnly: true
      containers:
        - name: httpbin
          image: kennethreitz/httpbin
          ports:
            - containerPort: 80
      volumes:
        - name: workload-socket                    # shim socket; Istio mounts it into istio-proxy
          emptyDir:
            medium: Memory
            sizeLimit: 1Mi
        - name: tbot-upstream                      # tbot socket; mounted only in the shim
          csi:
            driver: csi.spiffe.io
            readOnly: true
```

Make the same changes to `sleep` (`sleep-teleport-shim.yaml`), then:

```bash
kubectl apply -f httpbin-teleport-shim.yaml -f sleep-teleport-with-sds.yaml
kubectl -n app-teleport rollout status deploy/httpbin
kubectl -n app-teleport rollout status deploy/sleep
```

`app-citadel` workloads don't change.

## Step 5 - Verify

5.1–5.5 explain each check. To run them all at once, use [`verify-shim.sh`](verify-shim.sh) (5.6).

### 5.1 Pod layout and start order

```bash
POD=$(kubectl -n app-teleport get pod -l app=httpbin -o jsonpath='{.items[0].metadata.name}')
kubectl -n app-teleport get pod $POD -o jsonpath='init: {.spec.initContainers[*].name}{"\n"}main: {.spec.containers[*].name}{"\n"}'
# init: sds-shim istio-init          <- istio-init (or istio-validation) runs last in non-native mode
# main: httpbin istio-proxy          <- istio-proxy is a regular container
```

If `istio-proxy` shows up under `init:` (for example `init: istio-init istio-proxy sds-shim`), native-sidecar injection is still on for this pod. Check Step 4.1.

```bash
kubectl -n app-teleport logs $POD -c sds-shim
# sds-shim: /var/run/secrets/workload-spiffe-uds/socket -> /run/tbot-upstream/socket (ALL served as ROOTCA)

kubectl -n app-teleport logs $POD -c istio-proxy | grep -E "workload SDS socket|Skipping connecting to CA|generated new workload certificate"
# Existing workload SDS socket found at var/run/secrets/workload-spiffe-uds/socket ...
# Workload is using file mounted certificates. Skipping connecting to CA
```

`generated new workload certificate` in this log means istio-agent fetched certs from istiod, i.e. the fallback. Check the container order above.

### 5.2 The cert is Teleport-signed, with the pod's own identity

The SPIFFE ID alone proves nothing: an istiod fallback cert carries the same `spiffe://<tenant>/...` SAN. Check the **signature**:

```bash
export TP_TENANT=<tenant>
curl -s "https://$TP_TENANT/webapi/spiffe/bundle.json" \
  | jq -r '.keys[] | select(.use=="x509-svid") | .x5c[0]' \
  | while read -r b; do echo "-----BEGIN CERTIFICATE-----"; echo "$b" | fold -w 64; echo "-----END CERTIFICATE-----"; done \
  > teleport-root.pem

istioctl pc s deploy/httpbin.app-teleport -o json \
  | jq -r '.dynamicActiveSecrets[] | select(.name=="default") | .secret.tlsCertificate.certificateChain.inlineBytes' \
  | base64 -d > leaf.pem

openssl x509 -in leaf.pem -noout -issuer -dates -ext subjectAltName,authorityKeyIdentifier
openssl x509 -in teleport-root.pem -noout -ext subjectKeyIdentifier
openssl verify -CAfile teleport-root.pem            -untrusted leaf.pem leaf.pem   # leaf.pem: OK
openssl verify -CAfile certs/rootA/root-cert.pem    -untrusted leaf.pem leaf.pem   # error 20 ... verification failed
```

| Field | Teleport-issued (expected) | istiod fallback |
|---|---|---|
| Issuer | `O=<tenant>, CN=<tenant>` | `O=Istio, CN=Intermediate CA` |
| Authority Key Identifier | **equals** the Teleport root's Subject Key Identifier | equals the Istio intermediate's |
| Lifetime | ~1h | ~24h |
| `verify` against the Teleport root / Istio root | OK / fails | fails / OK |
| SAN | `spiffe://<tenant>/ns/app-teleport/sa/httpbin` | **the same**, so don't rely on it |

A SAN of `ns/default/sa/tbot` would mean the shim isn't in the workload pod (see [What it does and does not do](#what-it-does-and-does-not-do)).

### 5.3 `ROOTCA` now carries both trust domains

`istioctl` shows one `ROOTCA` row per trust domain:

```bash
istioctl pc s deploy/httpbin.app-teleport
# RESOURCE NAME  TYPE        STATUS  VALID CERT  ...  NOT AFTER   ...  TRUST DOMAIN
# default        Cert Chain  ACTIVE  true        ...  (~1h)      ...  <tenant>
# ROOTCA         CA          ACTIVE  true        ...  2036-...   ...  cluster.local
# ROOTCA         CA          ACTIVE  true        ...  2036-...   ...  <tenant>
```

Without the shim there is only the `<tenant>` row. The full secret is a SPIFFE validator with both domains:

```bash
istioctl pc s deploy/httpbin.app-teleport -o json \
  | jq '.dynamicActiveSecrets[] | select(.name=="ROOTCA") | .secret | del(..|.inlineBytes?)'
# "name": "ROOTCA"
# customValidatorConfig: envoy.tls.cert_validator.spiffe
#   trustDomains: [cluster.local, <tenant>]
```

There should be **no** `ALL` secret, because nothing requests it anymore.

### 5.4 All four directions, with no EnvoyFilters

```bash
kubectl get envoyfilter -A          # expect: No resources found

call() { kubectl -n "$1" exec deploy/sleep -c sleep -- curl -s -o /dev/null -w "%{http_code}\n" "http://httpbin.$2:8000/headers"; }
call app-teleport app-teleport   # 200
call app-citadel  app-citadel    # 200
call app-citadel  app-teleport   # 200
call app-teleport app-citadel    # 200

kubectl -n app-teleport exec deploy/sleep -c sleep -- curl -s http://httpbin.app-citadel:8000/headers \
  | jq -r '.headers["X-Forwarded-Client-Cert"][0]'
# By=spiffe://cluster.local/...;URI=spiffe://<tenant>/ns/app-teleport/sa/sleep
```

### 5.5 A/B: prove the shim is what fixes it

Re-apply the Setup.md Part 8 manifests (CSI volume directly as `workload-socket`, no shim), restart, and call across domains:

```bash
kubectl apply -f httpbin-teleport-basic.yaml -f sleep-teleport-basic.yaml
kubectl -n app-teleport rollout status deploy/httpbin; kubectl -n app-teleport rollout status deploy/sleep
call app-citadel app-teleport    # 503
call app-teleport app-citadel    # 503

kubectl apply -f httpbin-teleport-with-sds-shim.yaml -f sleep-teleport-with-sds.yaml
kubectl -n app-teleport rollout status deploy/httpbin; kubectl -n app-teleport rollout status deploy/sleep
call app-citadel app-teleport    # 200
call app-teleport app-citadel    # 200
```

## Security and isolation

| Concern | How it's handled |
|---|---|
| **Application traffic** | The shim is **not in the data path**. Requests and responses between workloads go through Envoy only; the shim never sees them. |
| **Network exposure** | No TCP listener and no outbound network connections. One Unix socket in, one Unix socket out. No `ports:` in the container spec. |
| **Who can reach the shim** | Its socket lives in a memory `emptyDir` mounted only by `sds-shim` and `istio-proxy`. The application container doesn't mount it. |
| **Who can reach tbot** | The CSI volume (`tbot-upstream`) is mounted **only** in the shim, read-only. The app container has no tbot socket at all (in README-1 it didn't either; the CSI volume was mounted only by `istio-proxy`). |
| **What it can read** | Only SDS messages. These **include the workload's SVID private key** (the `default` secret), the same material `istio-proxy` receives. The shim parses only each secret's `name`; it doesn't log, store or forward key material anywhere except to Envoy. |
| **Other containers** | No shared process namespace, no host paths, no Kubernetes API access. The code makes no API calls. The pod's service account token is still auto-mounted into every container by Kubernetes. If the app doesn't need it, set `automountServiceAccountToken: false` on the pod (`istio-proxy` uses its own projected `istio-token`). |
| **Container hardening** | `scratch` image (no shell, no package manager, no libc), non-root UID 65532, read-only root filesystem, all capabilities dropped, no privilege escalation, `RuntimeDefault` seccomp. |
| **Supply chain** | About 150 lines of Go. Dependencies: `grpc-go`, `go-control-plane` (Envoy API types), `protobuf`. Pin the image by digest in production. |

## Resource footprint

| | Value |
|---|---|
| Image | ~14 MB (static binary on `scratch`; most of it is protobuf descriptors from Envoy's API types) |
| Memory | Requests 16 Mi, limit 32 Mi, `GOMEMLIMIT=24MiB`. Expected RSS is around 10 MB; check with `kubectl top pod --containers`. |
| CPU | Request 5m. Idle except when tbot pushes an update (SVID renewal roughly hourly, bundle changes). |
| Connections | One SDS stream per Envoy subscription; one gRPC connection to tbot |

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| istio-proxy log: `Starting SDS server for workload certificates, will listen on var/run/secrets/workload-spiffe-uds/socket`, and the leaf cert is issued by `O=Istio` with ~24h validity | istio-proxy started before the shim was listening; istio-agent became the SDS server and got certs from istiod | Most often Istio injected `istio-proxy` as a **native sidecar ahead of `sds-shim`** (`init: istio-init istio-proxy sds-shim`). Set `ENABLE_NATIVE_SIDECARS=false` on `istiod-1-28-6` and/or add `sidecar.istio.io/nativeSidecar: "false"` to the pod template (Step 4.1), then delete the pod. Otherwise confirm that `sds-shim` has `restartPolicy: Always` and the startup probe |
| `sds-shim` crash-looping: `… is already being served by another process; istio-proxy probably started before sds-shim` | Same ordering problem; the shim refuses to take over istio-agent's socket | Same fix as the row above, then delete the pod |
| Pod stuck in `Init`, `sds-shim` startup probe failing | Shim can't create its socket | `kubectl logs -c sds-shim`; check the `workload-socket` mount isn't read-only |
| `default` cert SAN is `ns/default/sa/tbot` | The shim isn't in the workload pod (shared shim) | Run it per pod, as in Step 4 |
| `ROOTCA` still lists only `<tenant>` | tbot not serving the federated bundle | Re-run README-1 Part 7.2 / 7.3 against tbot directly |
| Envoy waiting on secrets / listeners not ready | Shim can't reach tbot (`/run/tbot-upstream/socket` missing) | Check the CSI volume and tbot DaemonSet on that node |
| Both `ROOTCA` and `ALL` secrets show up in Envoy | Part 9 EnvoyFilters still present | Step 3 |
| `unknown service` / `Unimplemented` errors in istio-proxy | Envoy is using an SDS API the shim doesn't implement (delta SDS) | Not expected with Istio's `sds-grpc` (`apiType: GRPC`); report the Envoy log |

The README-1 Part 7.3 probe still works for checking tbot directly, because it mounts the CSI volume itself, not the shim.

## Caveats

1. **Tested on one setup.** Verified on Teleport Cloud 18.11.2, Istio 1.28.5 / 1.28.6 and Kubernetes 1.33 (kind), plus the end-to-end unit test (fake tbot ↔ shim ↔ fake Envoy). Run `verify-shim.sh` (5.6) on every new cluster, Istio upgrade or Teleport upgrade.
2. **Stopgap.** If tbot adds a setting like SPIRE's `default_all_bundles_name`, set it, remove the shim, and switch the CSI volume back to `workload-socket`.
3. **`istio-proxy` can't be a native sidecar in `app-teleport`.** The shim itself needs Kubernetes 1.29+ (native sidecars), and `istio-proxy` must be a regular container so it starts after the shim (Step 4.1). The trade-offs are in the 4.1 table. Istio's injector can't place the shim ahead of a native `istio-proxy`.
4. **Identity path.** The shim relays (but doesn't inspect) workload private keys. Review the code and pin the image before production use.
5. **Scope.** Same as README-1: X.509-SVID mTLS between sidecars only. Parts 5–7 of README-1 (Citadel trusting Teleport, aliases, federation) are still required.
