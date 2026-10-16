# Istio + Teleport Workload Identity: Problem

A real interoperability incident has already caused mTLS handshake failures at the customer's Kubernetes cluster. Teleport tbot and Istio currently issue identities from separate CA chains.

# Istio + Teleport Workload Identity: Goal
**Goal:** one Kubernetes cluster with two kinds of workload identity that can talk to each other over mTLS.

## 1. Setting up in a nutshell

| Namespace | Istio revision | Who issues the identity | Trust domain |
|---|---|---|---|
| `app-citadel` | `1-28-5` | Istio's own CA (Citadel) | `cluster.local` |
| `app-teleport` | `1-28-6` | Teleport Enterprise Cloud (trial), via `tbot` | `green-pond.trial.teleport.sh` |

1. **Teleport Cloud:** log in, create a bot, a Kubernetes join token, a `WorkloadIdentity` template (`/ns/<ns>/sa/<sa>`) and a role for issuing it.
2. **tbot DaemonSet:** runs on every Kubernetes node, joins Teleport Cloud, and serves workload certificates on a node-local socket.
3. **SPIFFE CSI driver:** mounts that socket into `app-teleport` namespace pods. Istio's sidecar detects it and gets its certificate from Teleport instead of istiod.
4. **Istio CA you own:** a root and intermediate created by you and loaded as `cacerts`, with the **Teleport root appended**. This makes Citadel-side workloads trust Teleport identities.
5. **Two istiod revisions** One with trustDomain `cluster.local` and second with trustDomain `green-pond.trial.teleport.sh`. And with each other's trust domain as an alias, so identity names match across domains.
6. **`spiffe_federation` in Teleport (static CA bundle)** holding the Istio root, so Teleport-side workloads can trust Citadel identities.
7. **Workloads belong to two different revisions and STRICT mTLS in both namespaces.**

I would expect this to work. But only Teleport ↔ Teleport and Citadel ↔ Citadel worked, but **both cross-domain directions failed (503)**.

## 2. Why `Teleport spiffe_federation` with `Istio Multi-Root Support` not fixing it

Federation works as far as tbot:

```text
Istio cacerts holds teleport root and has Multi-Root Support ........... YES
Teleport Cloud spiffe_federation now holding Istio root ................ YES
tbot is syncing with Teleport cloud .................................... YES
Workload socket offers it .............................................. YES
Namespace `app-teleport`'s proxies gets Istio root ..................... NO (because it asks for "ROOTCA")
```

This a interoperability problem between Istio and tbot, not in Teleport's federation or Istio multi-root support.

## 3. The `ROOTCA` / `ALL` compatibility problem

Envoy fetches certificates from the socket **by name**:

| Name | Teleport (tbot) returns | Istio thinks it means |
|---|---|---|
| `default` | The workload's own certificate | Same |
| `ROOTCA` | Roots for **Teleport's own trust domain only** | "**Everything** this sidecar should trust" |
| `ALL` | Teleport roots **plus federated** roots (the Istio root) | Never used |

- default and ROOTCA are the names Istio's sidecar uses; SPIRE uses the same names as its defaults (and adds ALL also); tbot uses the same three names."
- Istio proxies always request the trust bundle as `ROOTCA` for Istio mTLS. The name is a constant in Istio's code, and no configuration option to make it request ALL.
- The Citadel side proxies already trusts Teleport because of step 4.
- The Teleport-side proxies trusts only Teleport and rejects Citadel peers in both directions. 

Istio proxies will not request `ALL`.  Istio handles multiple roots by putting all of them **inside** `ROOTCA`. So Istio proxies requests only `ROOTCA`. And `ROOTCA` has a different meaning in Teleport - which is `Roots for Teleport's own trust domain only`

Istio's SPIRE docs tell you to configure SPIRE to serve "all bundles" under the name `ROOTCA`. **tbot has no equivalent setting.** That missing setting is the interoperability gap.

## 4. Fix A: EnvoyFilters 

Two EnvoyFilters in `app-teleport` rewrite Envoy's config to ask for `ALL` instead of `ROOTCA`:

- **Inbound** (Teleport pod as server): fixes Citadel → Teleport.
- **Outbound** (Teleport pod as client): fixes Teleport → Citadel.

Tested both ways with A/B runs. Why it's hard to manage:

- **Per port on each workload:** every inbound port has its own config section. 
- **Per destination:** every Citadel service the Teleport side calls needs its own outbound entry. 
- **Tied to Istio & Envoy internals:** chain names, protocol lists and merge behavior can change in any Istio upgrade, and mistakes fail silently or get the whole config rejected.
- **Lots of verification:** after every Istio upgrade and every new service or port.

So maintenance is high.

## 5. Fix B: the SDS shim

A small sidecar between Envoy and tbot: Codebase is very small using Go.

```text
Envoy asks "ROOTCA"  ->  shim asks tbot for "ALL"  -> gets it -> shim returns it back named as "ROOTCA"
```


- **No EnvoyFilters.** It covers every port, every service and both directions automatically.
- **Must run in each pod.** tbot identifies a workload by the process connecting to it. A shared shim (per node, or in the tbot pod) would give every workload the shim's identity.
- **Must start before Istio's sidecar.** If it doesn't, Istio silently falls back to istiod certificates that look like Teleport ones. Istio always places its sidecar first in native-sidecar mode, so the fix is `ENABLE_NATIVE_SIDECARS=false` on the Teleport revision. The cost is losing native-sidecar benefits there.
- **Small and isolated:** ~14 MB image, ~16 MB memory, Unix sockets only, no network, not in the traffic path. It relays the workload's certificate and private key (default) and trust bundle (ROOTCA) to Envoy, the same material Envoy receives today. It doesn't log or store them.

Tested: Teleport-signed certificates (verified by signature), each pod's own identity, all four directions return 200. 

**Trade-off:** you own and maintain custom code until Teleport / Istio fixes the gap.

## 6. Fixing it properly: Teleport side or Istio side?

| | Teleport  | Istio  |
|---|---|---|
| What | A tbot setting for serving own + federated roots under the name `ROOTCA` | A setting to request `ALL` (or a configurable name) when using an external socket |
| Scope | TBD by Teleport | Touches how istiod builds every sidecar's MTLS config, inbound and outbound |
| History | Not sure Teleport followed Spire completely | Istio's docs mention SPIRE  |


**Teleport is the easier and the more appropriate side.**

- Istio's is strict on what goes in `ROOTCA`, and SPIRE follows it.
- tbot uses same names as SPIRE's names but not SPIRE's setting to remap them. Matching SPIRE is a small, well-precedented request.
- With it, no shim and no EnvoyFilters are needed: back to the plain CSI setup with native sidecars turned back on.

An Istio-side option would also be legitimate, but it's harder to land and broader than needed. 

## 7. What to check always whichever fix you use

- **Istio with multi-root trusting Teleport** (Teleport root in `cacerts` plus aliases) is always needed. Spiffe Federation needed at the Teleport side.
- **Static federation is manual:** when the Istio root rotates, update `spiffe_federation` before switching intermediates.
- **Silent fallback risk:** a misconfigured Teleport-side pod can get an istiod certificate carrying a Teleport-looking identity.
- **Scope:** X.509 mTLS between sidecars only, no JWTs.

