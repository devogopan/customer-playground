# validate-routes (Go) v2

Checks every route exposed through Istio gateways and reports one status per
check. Runs as a Kubernetes Job (`job.yaml`) or CronJob (`cronjob.yaml`) with
the existing `vs-route-validator` ServiceAccount, RBAC and PodMonitor.

## What it checks

For every VirtualService in the `envoy*` namespaces (or `--namespace`):

| Route | Check | Success |
|---|---|---|
| `http` match | `GET https://<host>[:<match.port>]<path>`; path = `uri.exact`, `uri.prefix`, or `/` when the match has no `uri` | the HTTP status |
| `tcp` match | TCP connect to `<host>:<match.port>` (443 if unset) | `200` |
| `tls` match | TLS handshake to `<host>:<match.port>` with SNI = host | `200` |

Not checked (counted in the summary and `visa_route_skipped_total`):
`mesh_only` (VirtualService with no gateway), `wildcard_host` (`*` in host),
`regex_match` (`uri.regex`, no concrete path). Identical checks are made once.

With `--bypass-lb`, connections for each namespace go to the node (`hostIP`)
of a **ready pod selected by the gateway Service** (`NodePort` or
`LoadBalancer` with an HTTPS nodePort), on that Service's nodePort for the
route's port. TLS SNI and the Host header remain the route hostname. The
chosen target is printed to stderr:

```
Bypass target for envoy-oi-system-p: service=istio-ingressgateway pod=istio-ingressgateway-7d9f... node=10.1.2.3 nodePorts=map[443:30443 80:30080]
```

If no target is found, a warning explains why and that namespace's routes
report `0`.

## Status codes

| Status | Meaning |
|---|---|
| `100`–`599` | HTTP status from the gateway (redirects are **not** followed by default) |
| `200` (tcp/tls) | connection / handshake succeeded |
| `0` | unclassified error, or no bypass target / nodePort for the namespace |
| `6000` | DNS resolution failed |
| `7000` | connection refused / host unreachable |
| `28000` | timed out (`--timeout`) |
| `35000` | TLS handshake failed (incl. certificate errors with `--verify-tls`) |
| `52000` | connection closed without an HTTP response |

## Flags

| Flag | Default | |
|---|---|---|
| `--namespace a,b` | | Namespaces to check; otherwise all namespaces starting with `--namespace-prefix` |
| `--namespace-prefix` | `envoy` | |
| `--bypass-lb` | off | Connect via the gateway pod's node and NodePort |
| `--concurrency` | `50` | Simultaneous checks |
| `--timeout` | `30s` | Per attempt |
| `--retries` | `2` | Attempts on connection errors (HTTP statuses are not retried) |
| `--follow-redirects` | off | Report the final status after redirects (v1 behaviour) |
| `--verify-tls` | off | Verify gateway certificates |
| `--csv-extended` | off | Append `type,path,port` to each stdout line |
| `--fail-on-error` | off | Exit 1 if any check failed (`0`, `>= 500`) or a namespace could not be listed |
| `--debug` | off | Log every attempt |
| `--version` | | Print version |

Environment: `SCRAPE_WAIT_SECONDS` (default 60), `RUN_ID` (default UTC `YYYYmmdd-HHMMSS`).

## Output

stdout, one line per check (unchanged format):
`run_id,namespace,hostname,status` — with `--csv-extended`:
`run_id,namespace,hostname,status,type,path,port`.

stderr summary:
```
Checked 21005 routes in 11.6s (concurrency=200)
Status counts: 200=19983 301=1 503=1020 6000=1 (failed=1021)
Skipped: mesh_only=1 regex_match=1 wildcard_host=1
```

Metrics on `:8000/metrics` (served after the checks, for `SCRAPE_WAIT_SECONDS`):

| Metric | Labels |
|---|---|
| `visa_route_status` | `namespace, hostname, type, path, run_id, bypass_lb` (**`path` is new**) |
| `visa_route_status_counter_total` | `run_id, namespace, status, bypass_lb` |
| `visa_route_skipped_total` (new) | `run_id, namespace, reason` |

Exit code: `1` if there is nothing to check (no namespaces / routes), or with
`--fail-on-error` as above; otherwise `0`.

## Changes from v1 (and the Python checker)

- Bypass mode: picks a ready **gateway** pod via the Service selector (required with `externalTrafficPolicy: Local`), uses its `hostIP`, accepts `LoadBalancer` Services, and logs why when no target is found.
- Errors are classified in both modes (previously `0` in standard mode; the Python bypass mode always reported `0`).
- Redirects are not followed: a `301`/`302` from the gateway is reported as such. Use `--follow-redirects` for the old behaviour.
- Prefix matches are checked at their prefix; regex matches, wildcard hosts and mesh-only VirtualServices are skipped; tcp/tls routes get TCP/TLS checks on their port; duplicate checks are removed.
- A namespace whose VirtualServices cannot be listed is logged and skipped instead of aborting the run.
- `visa_route_status` has a `path` label, so different paths of a host no longer overwrite each other.
- VirtualServices are read via `networking.istio.io/v1`, falling back to `v1beta1`.
- Manifests: `dnsConfig ndots:1` (avoids ~5 DNS queries per check), `backoffLimit: 0`, `activeDeadlineSeconds`, CronJob `concurrencyPolicy: Forbid`.

No RBAC changes are needed.

## Build

```bash
docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=v2.0.0-go \
  -t <registry>/validate-routes:v2.0.0-go --push .
```
