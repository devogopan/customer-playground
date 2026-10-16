# Istio + Teleport Enterprise Cloud Workload Identity (with Citadel side-by-side)

- **Teleport Enterprise Cloud (trial)** is the SPIFFE CA for workloads in `app-teleport`. No Teleport control plane runs in your cluster; only `tbot` does.
- **Istio Citadel**, running locally with a **plugged-in root CA** you own, is the CA for workloads in `app-citadel`.
- The two trust domains are wired to trust each other:
  - **Citadel → trusts Teleport**: the Teleport SPIFFE root is appended to Istio's `root-cert.pem` (multi-root, same technique as the [Tetrate root cert rotation guide](https://docs.tetrate.io/istio-subscription/howto/root-cert-rotation)).
  - **Teleport → trusts Citadel**: a `spiffe_federation` resource in Teleport Cloud (an Enterprise feature, included in the Cloud trial), plus two EnvoyFilters (inbound and outbound) / sds-shim sidecar - so that Istio's Envoy actually uses the federated bundle.

> **Tested results** (Teleport Cloud 18.11.2, Istio 1.28.5 / 1.28.6, kind), STRICT mTLS in both namespaces:
>
> | Call | Without Part 9 | With Part 9 |
> |---|---|---|
> | Teleport → Teleport | 200 | 200 |
> | Citadel → Citadel | 200 | 200 |
> | Citadel → Teleport | 503 | ✅ 200 (inbound filter / sds-shim) |
> | Teleport → Citadel | 503 | ✅ 200 (outbound filter / sds-shim) |
>
>
> Though SPIFFE federation from Teleport gets configured, there is a compatibility issue between Istio and Teleport in calling the Root CAs it should trust.

> With the workaround both cross-domain rows were A/B tested with fresh pods. `X-Forwarded-Client-Cert` showed the expected SPIFFE IDs from both trust domains.


## Table of Contents

- [Architecture](#architecture)
- [Trust Map](#trust-map)
- [Prerequisites](#prerequisites)
- [Part 1 - Connect to Teleport Enterprise Cloud](#part-1---connect-to-teleport-enterprise-cloud)
- [Part 2 - Bot, Join Token, WorkloadIdentity](#part-2---bot-join-token-workloadidentity)
- [Part 3 - Deploy tbot DaemonSet](#part-3---deploy-tbot-daemonset)
- [Part 4 - Install SPIFFE CSI Driver](#part-4---install-spiffe-csi-driver)
- [Part 5 - Create the Istio Root CA and cacerts](#part-5---create-the-istio-root-ca-and-cacerts)
- [Part 6 - Install Two Istio Revisions](#part-6---install-two-istio-revisions)
- [Part 7 - Federate Teleport with the Istio Trust Domain](#part-7---federate-teleport-with-the-istio-trust-domain)
- [Part 8 - Deploy Workloads and Confirm the Baseline Failure](#part-8---deploy-workloads-and-confirm-the-baseline-failure)
- [Part 9 - Make Teleport-side Sidecars Use Federated Bundles](#part-9---make-teleport-side-sidecars-use-federated-bundles)
- [References](#references)

## Architecture

```text
                    ┌───────────────────────────────────────────┐
                    │ Teleport Enterprise Cloud (<tenant>)      │
                    │  - SPIFFE CA (trust domain = <tenant>)    │
                    │  - spiffe_federation: cluster.local       │
                    │  - /webapi/spiffe/bundle.json             │
                    └──────────────▲────────────────────────────┘
                                   │ HTTPS 443 (outbound only)
┌──────────────────────────────────┼──────────────────────────────────────────┐
│ Local Kubernetes cluster         │                                          │
│                                  │                                          │
│  istio-system                    │                                          │
│  ┌─────────────────┐  ┌──────────┴───────┐   cacerts secret (plugged-in CA) │
│  │ istiod-1-28-5   │  │ istiod-1-28-6    │   root-cert.pem =                │
│  │ trustDomain:    │  │ trustDomain:     │     Istio root + Teleport root   │
│  │  cluster.local  │  │  <tenant>        │                                  │
│  └───────┬─────────┘  └───────┬──────────┘                                  │
│          │ xDS + certs        │ xDS only                                    │
│  ┌───────▼─────────┐  ┌───────▼──────────┐                                  │
│  │ ns app-citadel  │  │ ns app-teleport  │                                  │
│  │ sidecar certs   │  │ sidecar certs    │◄── SDS over UDS (CSI mount)      │
│  │ from Citadel    │  │ from tbot        │                                  │
│  └─────────────────┘  └──────────────────┘                                  │
│                                ▲                                            │
│                       ┌────────┴────────┐                                   │
│                       │ tbot DaemonSet  │── joins <tenant> via kubernetes   │
│                       │ /run/tbot/socket│   static_jwks join method         │
│                       └─────────────────┘                                   │
└─────────────────────────────────────────────────────────────────────────────┘
```


## Trust Map

| Connection | Who validates | Needs | Provided by | Status |
|---|---|---|---|---|
| `app-citadel` client → `app-teleport` server: server cert | Citadel sidecar | Teleport root | Combined `root-cert.pem` (Part 5) + `trustDomainAliases` (Part 6) | Tested, works |
| `app-citadel` client → `app-teleport` server: client cert | Teleport sidecar (inbound) | Istio root | `spiffe_federation` (Part 7) + inbound EnvoyFilter (Part 9) | Tested, works (A/B verified) |
| `app-teleport` client → `app-citadel` server: server cert | Teleport sidecar (outbound) | Istio root | `spiffe_federation` (Part 7) + outbound EnvoyFilter (Part 9.6) | Tested, works (A/B verified) |
| `app-teleport` client → `app-citadel` server: client cert | Citadel sidecar | Teleport root | Combined `root-cert.pem` + `trustDomainAliases` | Tested, works |

Why the Teleport side is harder: the `app-teleport` sidecars get their roots directly from tbot's Envoy SDS API. tbot serves three resource names ([docs](https://goteleport.com/docs/reference/machine-workload-identity/workload-identity/workload-identity-api-and-workload-attestation/)). Observed in Envoy's config dump:

| SDS name | Returned by tbot as | Trust domains |
|---|---|---|
| `default` | The workload's X509-SVID | n/a |
| `ROOTCA` | `envoy.tls.cert_validator.spiffe` | **Own trust domain only** |
| `ALL` | `envoy.tls.cert_validator.spiffe` | Own **plus every federated** trust domain |

Istio sidecars request `ROOTCA`, so the federated Citadel root never reaches Envoy unless something rewrites that name to `ALL` (Part 9). SPIRE can remap this name (`default_all_bundles_name`), but tbot currently has no equivalent option.


## Prerequisites

- A local Kubernetes cluster (kind/k3s/minikube) that can make **outbound HTTPS to `<tenant>.teleport.sh`**.
- Privileged pods and `hostPath` allowed (tbot, CSI driver).
- Local tools: `kubectl`, `helm`, `jq`, `openssl`, `python3`, `make`, `curl`.
- `tsh` and `tctl` installed locally, with a version compatible with your Cloud tenant ([downloads](https://goteleport.com/download/)).
- A Teleport Enterprise Cloud trial tenant. The trial is time-limited (typically 14 days), so plan your tests within that window.

Set these once per shell:

```bash
export TP_TENANT="example.teleport.sh"      # your tenant hostname, no https://
export TP_USER="you@example.com"             # your Teleport login
export TP_TRUST_DOMAIN="$TP_TENANT"          # Teleport cluster name == SPIFFE trust domain
export ISTIO_TRUST_DOMAIN="cluster.local"
```

Keep generated key material out of git:

```bash
cat >> .gitignore <<'EOF'
certs/
*.pem
*.key
Screenshot-*.png
EOF
```

## Part 1 - Connect to Teleport Enterprise Cloud

### 1.1 Log in

```bash
tsh login --proxy="$TP_TENANT:443" --user="$TP_USER"
tctl status
```

`tctl status` should print your tenant as the cluster name. That value is your Teleport SPIFFE trust domain. If it differs from `$TP_TENANT`, set `TP_TRUST_DOMAIN` to what `tctl status` prints.

### 1.2 Find the tenant version (for the tbot image tag)

```bash
TP_VERSION=$(curl -s "https://$TP_TENANT/webapi/ping" | jq -r '.server_version')
echo "$TP_VERSION"
```

Use the same version, or one major version older, for the `tbot` image in Part 3. Cloud tenants upgrade automatically, so re-check this if tbot starts failing later.

### 1.3 Check your permissions

The trial owner normally has the `editor` role. You need to be able to create `bot`, `token`, `role`, `workload_identity` and `spiffe_federation`:

```bash
tctl get roles --format=text | head
tctl get spiffe_federation   # an empty list is fine; "access denied" is not
```

## Part 2 - Bot, Join Token, WorkloadIdentity

### 2.1 Bot

`bot.yaml`:

```yaml
kind: bot
version: v1
metadata:
  name: istio-local
spec:
  roles: []
```

```bash
tctl create -f bot.yaml
```

### 2.2 Kubernetes join token (static JWKS)

tbot calls out to the Cloud tenant, which cannot reach your local API server, so use `static_jwks`.

Generate the token file directly from the cluster's JWKS. Don't paste the JWKS by hand: a placeholder or a mangled value fails the join with `parsing provided jwks: illegal base64 data at input byte 0`.

```bash
kubectl get --raw /openid/v1/jwks > jwks.json
jq -e '.keys[0].n' jwks.json >/dev/null && echo "JWKS OK"

cat > bot-token.yaml <<EOF
kind: token
version: v2
metadata:
  name: istio-local-bot
spec:
  roles: [Bot]
  bot_name: istio-local
  join_method: kubernetes
  kubernetes:
    type: static_jwks
    static_jwks:
      jwks: '$(jq -c . jwks.json)'
    allow:
      - service_account: "default:tbot"
EOF

tctl create -f bot-token.yaml --force
```

Check what the tenant stored:

```bash
tctl get token/istio-local-bot --format=json \
  | jq -r '.[0].spec.kubernetes.static_jwks.jwks' | jq '.keys[] | {kid, alg, n: .n[0:20], e}'
```

> If you recreate the cluster, its service account signing key changes. Re-run this step, then `kubectl -n default rollout restart ds/tbot-wi`.

### 2.3 WorkloadIdentity

`workload-identity.yaml`:

```yaml
kind: workload_identity
version: v1
metadata:
  name: istio-workload-identity
  labels:
    wi: istio-like
spec:
  spiffe:
    id: /ns/{{ workload.kubernetes.namespace }}/sa/{{ workload.kubernetes.service_account }}
```

### 2.4 Issuer role, attached to the bot

`workload-identity-issuer-role.yaml`:

```yaml
kind: role
version: v6
metadata:
  name: istio-workload-identity-issuer
spec:
  allow:
    workload_identity_labels:
      wi: ["istio-like"]
    rules:
      - resources: [workload_identity]
        verbs: [list, read]
```

```bash
tctl create -f workload-identity.yaml
tctl create -f workload-identity-issuer-role.yaml
tctl bots update istio-local --add-roles istio-workload-identity-issuer
```

## Part 3 - Deploy tbot DaemonSet

Reuse the chart from [README.md Part 3](README.md#part-3---deploy-tbot-daemonset-workload-identity-api). Copy `Chart.yaml` and the three templates (`configmap.yaml`, `daemonset.yaml`, `rbac.yaml`) unchanged. Only `values.yaml` changes:

```yaml
# tbot-wi-ds/values.yaml
clusterName: "example.teleport.sh"              # = $TP_TRUST_DOMAIN (also the join token audience)
teleportProxyAddress: "example.teleport.sh:443" # = $TP_TENANT:443
token: "istio-local-bot"

image:
  repository: public.ecr.aws/gravitational/tbot-distroless
  tag: "18.x.y"                                 # = $TP_VERSION from Part 1.2

extraArgs: []                                   # no --insecure: Cloud has a publicly trusted cert

workloadIdentityName: "istio-workload-identity"

hostSocketDir: "/run/tbot"
socketFile: "socket"                            # Istio expects exactly this name

serviceAccountName: "tbot"
namespace: "default"

hostPID: true
```

Install by templating values from your shell:

```bash
helm upgrade --install tbot-wi ./tbot-wi-ds -n default \
  --set clusterName="$TP_TRUST_DOMAIN" \
  --set teleportProxyAddress="$TP_TENANT:443" \
  --set image.tag="$TP_VERSION"
kubectl -n default rollout status ds/tbot-wi
kubectl -n default logs -l app=tbot-wi --tail=30
```

A healthy start logs these messages, in this order:

```text
"message":"Fetched new bot identity" ... roles=[bot-istio-local]          # kubernetes join succeeded
"message":"Successfully initialized trust bundle cache"                   # SPIFFE bundle fetched from Cloud
"message":"Listener opened for Workload API endpoint","addr":"/run/tbot/socket"
"message":"Sent heartbeat" ... join_method:"kubernetes"
```

The warning `Bot is configured with a non-persistent storage destination` is expected with `storage: memory`: each pod restart simply re-joins. The bot should also show up in Cloud:

```bash
tctl bots instances ls istio-local
```

## Part 4 - Install SPIFFE CSI Driver

Identical to [README.md Part 4](README.md#part-4---install-spiffe-csi-driver):

```bash
kubectl create namespace spiffe-csi
kubectl apply -f spiffe-csi-driver.yaml
kubectl -n spiffe-csi rollout status ds/spiffe-csi-driver
```

## Part 5 - Create the Istio Root CA and cacerts

This follows the [Tetrate root cert rotation guide](https://docs.tetrate.io/istio-subscription/howto/root-cert-rotation), using upstream Istio's cert Makefiles. Owning the Istio root lets you (a) give it to Teleport for federation and (b) rotate it later without downtime.

### 5.1 Generate root + intermediate

```bash
mkdir -p certs && cd certs
curl -sLO https://raw.githubusercontent.com/istio/istio/release-1.28/tools/certs/Makefile.selfsigned.mk
curl -sLO https://raw.githubusercontent.com/istio/istio/release-1.28/tools/certs/common.mk

make -f Makefile.selfsigned.mk root-ca
make -f Makefile.selfsigned.mk intermediateA-cacerts

mkdir -p rootA && mv root-* rootA/ && mv intermediateA rootA/
cd ..
```

Result:

```text
certs/rootA/root-cert.pem            # Istio root (goes to Teleport federation)
certs/rootA/root-key.pem             # keep offline
certs/rootA/intermediateA/ca-cert.pem
certs/rootA/intermediateA/ca-key.pem
certs/rootA/intermediateA/cert-chain.pem
certs/rootA/intermediateA/root-cert.pem
```

### 5.2 Fetch the Teleport SPIFFE root as PEM

Teleport publishes its trust bundle (SPIFFE JWKS) at `/webapi/spiffe/bundle.json`:

```bash
curl -s "https://$TP_TENANT/webapi/spiffe/bundle.json" \
  | jq -r '.keys[] | select(.use=="x509-svid") | .x5c[0]' \
  | while read -r b64; do
      echo "-----BEGIN CERTIFICATE-----"
      echo "$b64" | fold -w 64
      echo "-----END CERTIFICATE-----"
    done > certs/teleport-root.pem

openssl x509 -in certs/teleport-root.pem -noout -subject -issuer -ext subjectAltName
# Expect a URI SAN of spiffe://$TP_TRUST_DOMAIN
```

If the bundle has more than one key (for example during a Teleport CA rotation), all of them land in `teleport-root.pem`. That is what you want.

### 5.3 Build the combined root and create `cacerts`

```bash
cat certs/rootA/root-cert.pem certs/teleport-root.pem > certs/combined-root.pem

kubectl create namespace istio-system || true
kubectl -n istio-system delete secret cacerts --ignore-not-found
kubectl -n istio-system create secret generic cacerts \
  --from-file=certs/rootA/intermediateA/ca-cert.pem \
  --from-file=certs/rootA/intermediateA/ca-key.pem \
  --from-file=root-cert.pem=certs/combined-root.pem \
  --from-file=certs/rootA/intermediateA/cert-chain.pem
```

Both istiod revisions in `istio-system` read this same secret. `app-teleport` sidecars still validate **istiod's xDS serving cert** using `istio-ca-root-cert`, so both revisions must publish the same root ConfigMap.

> **Side effect:** `istiod-1-28-6` can also sign workload certs with this intermediate. A `1-28-6` pod that is **missing the `workload-socket` volume** does not fail. It silently gets an Istio-signed cert carrying a **Teleport** SPIFFE ID (`spiffe://<tenant>/...`) with Citadel's ~24h lifetime. Part 10.1 shows how to spot this.

## Part 6 - Install Two Istio Revisions

```bash
helm repo add istio https://istio-release.storage.googleapis.com/charts
helm repo update

helm install istio-base istio/base -n istio-system --version 1.28.6 \
  --set defaultRevision=1-28-6 || true
```

### 6.1 `istiod-1-28-5` (Citadel)

```bash
helm install istiod-1-28-5 istio/istiod -n istio-system --version 1.28.5 \
  --set revision=1-28-5 \
  --set meshConfig.accessLogFile=/dev/stdout \
  --set meshConfig.trustDomain="$ISTIO_TRUST_DOMAIN" \
  --set "meshConfig.trustDomainAliases[0]=$TP_TRUST_DOMAIN" \
  --set pilot.env.ISTIO_MULTIROOT_MESH=true \
  --set-string meshConfig.defaultConfig.proxyMetadata.PROXY_CONFIG_XDS_AGENT=true \
  --wait
```

### 6.2 `istiod-1-28-6` (Teleport identities)

Do not set `CA_ADDR` or `CA_PROVIDER`.

```bash
helm install istiod-1-28-6 istio/istiod -n istio-system --version 1.28.6 \
  --set revision=1-28-6 \
  --set meshConfig.accessLogFile=/dev/stdout \
  --set meshConfig.trustDomain="$TP_TRUST_DOMAIN" \
  --set "meshConfig.trustDomainAliases[0]=$ISTIO_TRUST_DOMAIN" \
  --set pilot.env.ISTIO_MULTIROOT_MESH=true \
  --set-string meshConfig.defaultConfig.proxyMetadata.PROXY_CONFIG_XDS_AGENT=true \
  --wait
```

Why each setting:

- `trustDomainAliases`: Istio builds the expected peer SAN as `spiffe://<own-trust-domain>/ns/<ns>/sa/<sa>`. The alias makes it also accept the other trust domain's SPIFFE ID, and makes `AuthorizationPolicy` principals match across domains.
- `ISTIO_MULTIROOT_MESH` and `PROXY_CONFIG_XDS_AGENT`: enable multi-root trust handling, as in the Tetrate guide. These are needed for zero-downtime rotation in Part 11.

### 6.3 Verify the root ConfigMap carries both roots

```bash
kubectl -n istio-system get cm istio-ca-root-cert -o jsonpath='{.data.root-cert\.pem}' \
  | awk '/BEGIN CERT/{n++} END{print n" certs"}'
# Expect: 2 certs (Istio root + Teleport root)

kubectl -n istio-system logs deploy/istiod-1-28-5 | grep -i "cacerts\|plugged-in\|root cert" | head
```

## Part 7 - Federate Teleport with the Istio Trust Domain

Requires Enterprise; the Cloud trial includes it.

Istiod does **not** serve a SPIFFE Bundle Endpoint, so the `https_web` source shown in the [Teleport federation docs](https://goteleport.com/docs/machine-workload-identity/workload-identity/federation/) has nothing to point at. Use the `static` bundle source instead (defined in Teleport's [`federation.proto`](https://github.com/gravitational/teleport/blob/master/api/proto/teleport/machineid/v1/federation.proto)). Teleport calls it "an option of last resort" because you must update it by hand when the Istio root rotates.

### 7.1 Convert the Istio root to a SPIFFE JWKS bundle

`pem-to-spiffe-jwks.py` (stdlib + `openssl` only; assumes RSA roots, which is the Istio Makefile default):

```python
#!/usr/bin/env python3
"""Convert PEM root certificate(s) into a SPIFFE trust bundle (JWKS)."""
import base64
import json
import re
import subprocess
import sys


def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def openssl(args, pem: str) -> bytes:
    return subprocess.run(["openssl", "x509", *args], input=pem.encode(),
                          capture_output=True, check=True).stdout


pems = re.findall(r"-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----",
                  open(sys.argv[1]).read(), re.S)
keys = []
for pem in pems:
    der = openssl(["-outform", "DER"], pem)
    modulus = openssl(["-noout", "-modulus"], pem).decode().strip().split("=", 1)[1]
    exponent = int(re.search(r"Exponent: (\d+)",
                             openssl(["-noout", "-text"], pem).decode()).group(1))
    keys.append({
        "use": "x509-svid",
        "kty": "RSA",
        "n": b64url(bytes.fromhex(modulus)),
        "e": b64url(exponent.to_bytes((exponent.bit_length() + 7) // 8, "big")),
        "x5c": [base64.b64encode(der).decode()],
    })

print(json.dumps({"keys": keys, "spiffe_refresh_hint": 300}))
```

```bash
python3 pem-to-spiffe-jwks.py certs/rootA/root-cert.pem > certs/istio-bundle.json
jq '.keys | length' certs/istio-bundle.json   # 1
```

### 7.2 Create the `spiffe_federation`

The resource name **must be the remote trust domain**:

```bash
cat > spiffe-federation-istio.yaml <<EOF
kind: spiffe_federation
version: v1
metadata:
  name: ${ISTIO_TRUST_DOMAIN}
spec:
  bundle_source:
    static:
      bundle: '$(cat certs/istio-bundle.json)'
EOF

tctl create -f spiffe-federation-istio.yaml
tctl get spiffe_federation/${ISTIO_TRUST_DOMAIN}
```

The status should show `current_bundle` and `current_bundle_synced_at`. The `x5c` cert in `current_bundle` must be the Istio root (`O=Istio, CN=Root CA`).

### 7.3 Verify tbot serves the federated bundle

This asks tbot's SPIFFE Workload API directly, without involving Istio. The `spire-agent` image is distroless (no shell), so it runs as an init container and `busybox` reads the output:

```bash
kubectl -n app-teleport apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: spiffe-probe
  annotations:
    sidecar.istio.io/inject: "false"
spec:
  serviceAccountName: sleep
  restartPolicy: Never
  initContainers:
    - name: fetch
      image: ghcr.io/spiffe/spire-agent:1.11.0
      command: ["/opt/spire/bin/spire-agent"]
      args: ["api", "fetch", "x509", "-socketPath", "/spiffe/socket", "-write", "/out"]
      volumeMounts:
        - { name: spiffe, mountPath: /spiffe, readOnly: true }
        - { name: out, mountPath: /out }
  containers:
    - name: shell
      image: busybox:1.36
      command: ["sleep", "3600"]
      volumeMounts:
        - { name: out, mountPath: /out }
  volumes:
    - name: spiffe
      csi: { driver: csi.spiffe.io, readOnly: true }
    - name: out
      emptyDir: {}
EOF

kubectl -n app-teleport wait --for=condition=Ready pod/spiffe-probe --timeout=60s
kubectl -n app-teleport logs spiffe-probe -c fetch
for f in bundle.0.pem federated_bundle.0.0.pem; do
  echo "== $f"
  kubectl -n app-teleport exec spiffe-probe -c shell -- cat /out/$f | openssl x509 -noout -subject -serial
done
kubectl -n app-teleport delete pod spiffe-probe
```

Expected:

```text
SPIFFE ID:  spiffe://<tenant>/ns/app-teleport/sa/sleep
[spiffe://cluster.local] CA #1 Valid After: ...
== bundle.0.pem              subject=O=<tenant>, CN=<tenant>, ...
== federated_bundle.0.0.pem  subject=O=Istio, CN=Root CA
```

The bot needs no extra role for this. `istio-workload-identity-issuer` from Part 2.4 was enough.

## Part 8 - Deploy Workloads and Confirm the Baseline Failure

### 8.1 Namespaces and workloads

Use the manifests from [README.md Part 7](README.md#part-7---deploy-workloads) and [Part 9.1](README.md#91-deploy-a-client-sleep-in-app-teleport):

```bash
kubectl create namespace app-citadel  && kubectl label namespace app-citadel  istio.io/rev=1-28-5
kubectl create namespace app-teleport && kubectl label namespace app-teleport istio.io/rev=1-28-6

kubectl apply -f httpbin-teleport-basic.yaml
kubectl apply -f httpbin-citadel-basic.yaml
kubectl apply -f sleep-teleport-basic.yaml
kubectl apply -f sleep-citadel-basic.yaml

```

> **Every `app-teleport` Deployment needs both of these in its pod template.** Without them, the pod silently falls back to an Istio-signed cert (see the note in Part 5.3):
>
> ```yaml
>       annotations:
>         inject.istio.io/templates: "sidecar"
> ...
>       volumes:
>         - name: workload-socket
>           csi:
>             driver: csi.spiffe.io
>             readOnly: true
> ```
>

### 8.2 Enforce STRICT mTLS

```bash
kubectl apply -f - <<'EOF'
apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata: { name: default, namespace: app-citadel }
spec: { mtls: { mode: STRICT } }
---
apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata: { name: default, namespace: app-teleport }
spec: { mtls: { mode: STRICT } }
EOF
```

### 8.3 Confirm each side's identity source

```bash
istioctl pc s deploy/sleep.app-teleport
istioctl pc s deploy/sleep.app-citadel
```

| | `app-teleport` | `app-citadel` |
|---|---|---|
| `default` validity | ~1h (Teleport SVID) | ~24h (Citadel) |
| TRUST DOMAIN column | `<tenant>` | empty |

`istioctl` shows the serial of the **first** root only, so both pods may show the same `ROOTCA` serial. Count the roots instead:

```bash
istioctl pc s deploy/sleep.app-citadel -o json \
  | jq -r '.dynamicActiveSecrets[] | select(.name=="ROOTCA") | .secret.validationContext.trustedCa.inlineBytes' \
  | base64 -d | grep -c BEGIN
# 2  (Teleport root + Istio root, from the combined root-cert.pem)

istioctl pc s deploy/sleep.app-teleport -o json \
  | jq '.dynamicActiveSecrets[] | select(.name=="ROOTCA") | .secret | del(..|.inlineBytes?)'
# SPIFFECertValidatorConfig with trustDomains: [<tenant>] only
```

### 8.4 Baseline: cross-domain calls fail in both directions

```bash
call() { kubectl -n "$1" exec deploy/sleep -c sleep -- curl -s -o /dev/null -w "%{http_code}\n" "http://httpbin.$2:8000/headers"; }

call app-teleport app-teleport   # 200
call app-citadel  app-citadel    # 200
call app-citadel  app-teleport   # 503: Teleport server rejects the Citadel client cert
call app-teleport app-citadel    # 503: Teleport client rejects the Citadel server cert
```

The Citadel → Teleport failure shows in the client sidecar log as:

```text
"GET /headers HTTP/1.1" 503 UC upstream_reset_before_response_started{connection_termination}
```

The Citadel side already trusts Teleport (Part 5). Both failures come from the **Teleport** side, whose `ROOTCA` contains only the Teleport trust domain, even though `spiffe_federation` is synced and tbot serves the Istio root (Part 7.3).

## Part 9 - Make Teleport-side Sidecars Use Federated Bundles

Two EnvoyFilters in `app-teleport` switch the SDS validation secret from `ROOTCA` to `ALL`:

| Filter | Applies to | Fixes |
|---|---|---|
| `teleport-inbound` | Inbound `FILTER_CHAIN`s (the Teleport pod as **server**) | Citadel → Teleport |
| `teleport-outbound` | Outbound `CLUSTER`s to Citadel services (the Teleport pod as **client**) | Teleport → Citadel |

Each one changes a single field. Everything else Istio generates (certs, SAN matchers, ALPN, TLS params) is kept.

### 9.1 How Istio merges the patches

Both filters rely on the same merge rule. **`MERGE` deep-merges `typed_config`: single values (like the SDS secret `name`) are overwritten, and lists are appended.**

### 9.2 Inbound: the EnvoyFilter

```yaml
# envoyfilter-teleport-inbound.yaml
---
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: teleport-inbound
  namespace: app-teleport
spec:
  configPatches:
    # Target the explicit port 8080 filter chain
    - applyTo: FILTER_CHAIN
      match:
        context: SIDECAR_INBOUND
        listener:
          name: virtualInbound
          filterChain:
            name: "0.0.0.0_80"
            transportProtocol: tls
      patch:
        operation: MERGE
        value:
          transport_socket:
            name: envoy.transport_sockets.tls
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
              common_tls_context:
                combined_validation_context:
                  validation_context_sds_secret_config:
                    name: ALL
---
```


```bash
kubectl apply -f envoyfilter-teleport-inbound.yaml
```


### 9.3 Outbound: inspect the cluster: the EnvoyFilter

Without the outbound filter, the Teleport client rejects the Citadel server cert:

```bash
kubectl -n app-teleport delete pod -l app=sleep && kubectl -n app-teleport rollout status deploy/sleep
kubectl -n app-teleport exec deploy/sleep -c sleep -- curl -s -o /dev/null -w "%{http_code}\n" http://httpbin.app-citadel:8000/headers
# 503
```


```yaml
# envoyfilter-teleport-outbound.yaml
---
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: teleport-outbound
  namespace: app-teleport
spec:
  configPatches:
    # One entry per Citadel-side service:port. Never leave `cluster` unscoped:
    - applyTo: CLUSTER
      match:
        context: SIDECAR_OUTBOUND
        cluster:
          service: httpbin.app-citadel.svc.cluster.local
          portNumber: 8000
      patch:
        operation: MERGE
        value:
          transport_socket:
            name: envoy.transport_sockets.tls          # selects the tlsMode
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
              common_tls_context:
                combined_validation_context:
                  validation_context_sds_secret_config:
                    name: ALL
---            
```

The type is `UpstreamTlsContext` here, not the `DownstreamTlsContext` from the inbound filter. Istio merges the two `Any` values only when their `@type` matches.

```bash
kubectl apply -f envoyfilter-teleport-outbound.yaml
```

### 9.4 Outbound: verify

```bash
# 1. No CDS NACK
kubectl -n app-teleport logs deploy/sleep -c istio-proxy --since=2m | grep -i rejected || echo "no NACK"

# 2. Teleport -> Citadel now works over mTLS
kubectl -n app-teleport exec deploy/sleep -c sleep -- curl -s http://httpbin.app-citadel:8000/headers \
  | jq -r '.headers["X-Forwarded-Client-Cert"][0]'
# By=spiffe://cluster.local/ns/app-citadel/sa/httpbin;...;URI=spiffe://<tenant>/ns/app-teleport/sa/sleep
```

### 9.4 Complete test: verify

```bash
call() { kubectl -n "$1" exec deploy/sleep -c sleep -- curl -s -o /dev/null -w "%{http_code}\n" "http://httpbin.$2:8000/headers"; }

call app-teleport app-teleport   # 200
call app-citadel  app-citadel    # 200
call app-citadel  app-teleport   # 503: Teleport server rejects the Citadel client cert
call app-teleport app-citadel    # 503: Teleport client rejects the Citadel server cert
```


### 9.5 Alternatives to the EnvoyFilters

1. **Feature request to Teleport:** a tbot option to serve federated bundles under `ROOTCA` (the equivalent of SPIRE's `default_all_bundles_name`). That would remove both EnvoyFilters and cover every port and service.
2. **SDS shim (DIY until then):** a small **per-pod** native sidecar between Envoy and tbot that rewrites the requested SDS name `ROOTCA` → `ALL` and renames the response back to `ROOTCA`. It must run in each workload pod: a shared shim (per node, or in the tbot pod) would make tbot attest the shim instead of the workload. Code, Dockerfile and steps: [README-2.md](README-2.md).


## References

- Base guide (self-hosted OSS): [README.md](README.md) / https://github.com/devogopan/teleport-istio-workload-identity
- Tetrate: Zero-downtime root cert rotation: https://docs.tetrate.io/istio-subscription/howto/root-cert-rotation
- Teleport SPIFFE Federation: https://goteleport.com/docs/machine-workload-identity/workload-identity/federation/
- Teleport Workload Identity API & attestation (SDS `default` / `ROOTCA` / `ALL`): https://goteleport.com/docs/reference/machine-workload-identity/workload-identity/workload-identity-api-and-workload-attestation/
- Teleport `federation.proto` (`static` bundle source): https://github.com/gravitational/teleport/blob/master/api/proto/teleport/machineid/v1/federation.proto
- Istio plug-in CA certs: https://istio.io/latest/docs/tasks/security/cert-management/plugin-ca-cert/
- Istio cert Makefiles: https://github.com/istio/istio/tree/master/tools/certs
- Istio SPIRE integration: https://istio.io/latest/docs/ops/integrations/spire/
