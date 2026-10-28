# Route checker: Go v2.0.0 vs. the Python checker

Image: `devogopan/validate-routes:v2.0.0-go` (linux/amd64, linux/arm64).
It uses the same ServiceAccount, RBAC and PodMonitor as before (no RBAC changes), and the same flags (`--namespace`, `--bypass-lb`, `--debug`) plus new optional ones.

## Speed

| | Python | Go v2 |
|---|---|---|
| How checks run | Batches of 50, 1s sleep between batches, each batch waits for its slowest route | `--concurrency` workers; a slow route holds up only its own worker |
| 21,000 routes, all responding | 7m 24s | 10–12s (`--concurrency 200`) |
| One route hanging until the timeout | Stalls its batch of 50 for ~61s, every time it occurs | Adds nothing; the run lasts at most as long as its slowest route (~61s) |
| Memory | — | ~100–150 MB at `--concurrency 200` |

## Bypass mode (`--bypass-lb`)

| Python | Go v2 |
|---|---|
| Connects to the node of the **first running pod** in the namespace, whether or not it is a gateway pod | Connects to the node of a **ready pod selected by the gateway Service**, which is required with `externalTrafficPolicy: Local` |
| Connects by node name (must resolve in DNS; IPv4 only) | Connects to the pod's `hostIP` (no DNS; IPv4 and IPv6) |
| Only `NodePort` Services are found; otherwise silently uses node:443 | `NodePort` or `LoadBalancer` Services; the nodePort is looked up per route port |
| No pod or no Service found: every route reports `0` silently | Prints the target in use, or a warning saying why there is none, e.g. `no ready pods match service gw selector istio=... (0 pods found)` |
| Retries never ran (2nd attempt always failed with "Session is closed") | Retries work |
| Error codes `7000/28000/35000` never appeared; every failure was `0` | Error codes are reported (see below) |
| Namespace without a running pod: rows repeated one hostname, other hosts missing | One correct row per route |

## Status codes

Errors now have codes in **both** modes. Previously standard mode reported `0` for any error, and bypass mode effectively did too.

| Status | Meaning |
|---|---|
| HTTP code | Response from the gateway. **Redirects are no longer followed**, so a `301`/`302` from the gateway is reported as is (`--follow-redirects` restores the old behaviour) |
| `6000` | DNS resolution failed (new) |
| `7000` | Connection refused / unreachable |
| `28000` | Timed out |
| `35000` | TLS handshake failed |
| `52000` | Connection closed without an HTTP response (new) |
| `0` | Other error, or no bypass target for the namespace |

## Which routes are checked

| Python | Go v2 |
|---|---|
| `uri.exact` path, otherwise `/` (prefix routes were checked at `/`) | `uri.exact`, or `uri.prefix`, or `/` when the match has no URI |
| `uri.regex` matches checked at `/` | Skipped (no concrete path); counted |
| Wildcard hosts (`*.example.com`) requested literally, always failing | Skipped; counted |
| VirtualServices not bound to a gateway (mesh-only) checked, always failing | Skipped; counted |
| `tcp` / `tls` routes checked with an HTTPS GET on 443 | `tcp`: TCP connect to the match port. `tls`: TLS handshake with SNI = host on the match port. `200` = success |
| Same host + path from several VirtualServices checked repeatedly | Checked once |
| Error listing one namespace aborted the whole run | Logged; the other namespaces are still checked |
| `networking.istio.io/v1beta1` only | `v1`, falling back to `v1beta1` |

## Output

- **stdout:** unchanged, `run_id,namespace,hostname,status`. The optional `--csv-extended` appends `type,path,port`.
- **stderr summary:**
  ```
  Checked 21005 routes in 11.6s (concurrency=200)
  Status counts: 200=19983 301=1 503=1020 6000=1 (failed=1021)
  Skipped: mesh_only=1 regex_match=1 wildcard_host=1
  ```
- **Metrics:**
  - `visa_route_status` has a new **`path`** label, so different paths of a host no longer overwrite each other.
  - `visa_route_status_counter_total` is unchanged.
  - `visa_route_skipped_total{run_id,namespace,reason}` is new.
  - Python's `visa_route_status_counter_created` series is no longer exported.
- **Exit code:**
  - `1` when there is nothing to check (previously `0`).
  - With `--fail-on-error`, `1` when any check fails (`0`, `>= 500`) or a namespace could not be listed.

## New optional flags

`--concurrency` (50), `--timeout` (30s), `--retries` (2), `--namespace-prefix` (envoy), `--follow-redirects`, `--verify-tls`, `--csv-extended`, `--fail-on-error`, `--version`.

## Manifests

- `job.yaml` / `cronjob.yaml`:
  - `dnsConfig ndots:1`. Route hostnames resolve in 1 DNS query instead of up to 5.
  - `backoffLimit: 0`. A failed run is not repeated 6 times.
  - `activeDeadlineSeconds: 1800`.
- `cronjob.yaml`: `concurrencyPolicy: Forbid`, so runs never overlap.
- `command:` and `PYTHONUNBUFFERED` are removed; the image runs the checker directly.

## What to expect when comparing with earlier Python results

- Failures show a code (`6000`, `7000`, `28000`, `35000`, `52000`) instead of `0`.
- Routes that redirect show `301`/`302` instead of the redirect target's status.
- Wildcard, regex and mesh-only routes no longer appear. TCP/TLS routes and prefix routes are now checked properly, so their results can differ.
- Fewer rows when the same host and path appeared in several VirtualServices.

Compare pre- and post-upgrade runs **of the same version**.

## Tested

- **Unit tests:** 23 tests, run with the race detector. They cover:
  - Gateway pod selection: a non-gateway pod that sorts first, pods that are not Ready, `LoadBalancer` Services.
  - Every error code, and retries (on errors, not on HTTP statuses).
  - SNI and Host header in bypass mode.
  - Redirects, TCP and TLS checks, and `--verify-tls`.
  - Route parsing, duplicate removal, the v1beta1 fallback, and namespace errors.
  - Output format and metrics.
- **End to end:** on a kind cluster with Istio, 2 NodePort gateways and 700 VirtualServices (21,000 routes), plus edge-case routes.
  - Both modes: all 21,000 regular routes gave the same result as the Python checker.
  - Edge routes gave their expected result (prefix `200`, redirect `301`, TCP `200`, TLS passthrough `200`, no DNS `6000`).
  - Bypass picked the gateway pod over another pod in the same namespace.
  - A gateway Service without ready pods produced the warning above.
  - A hanging route was reported as `28000` without delaying the rest of the run.
  - The metrics endpoint and the exit codes behaved as described above.
