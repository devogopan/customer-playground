import os
import logging
import time
import sys
import asyncio
import aiohttp
import argparse
from datetime import datetime
from typing import List, Optional
from kubernetes import client, config
from kubernetes.client.rest import ApiException
from prometheus_client import start_http_server, Gauge, Counter
# --- Configuration ---
METRICS_PORT = 8000
SCRAPE_WAIT_SECONDS = int(os.environ.get("SCRAPE_WAIT_SECONDS", "60"))
BATCH_SIZE = 50  # Tuning for reliability
DEFAULT_HTTPS_PORT = 443
REQUEST_TIMEOUT = 30
def generate_run_id():
    """Generate a UTC datetime string as run_id."""
    return datetime.utcnow().strftime('%Y%m%d-%H%M%S')
RUN_ID = os.environ.get("RUN_ID")
if not RUN_ID:
    RUN_ID = generate_run_id()
    generated_run_id = True
else:
    generated_run_id = False
# Will be configured later based on debug flag
logging_configured = False
def load_kube_config():
    try:
        config.load_incluster_config()
    except config.config_exception.ConfigException:
        config.load_kube_config()
def log_status_stdout(run_id, namespace, hostname, status):
    print(f"{run_id},{namespace},{hostname},{status}", file=sys.stdout, flush=True)
async def check_https_async(session, host, path="/", retries=2):
    """Test route using aiohttp for standard validation."""
    url = f"https://{host}{path}"
    logging.debug(f"Executing aiohttp request: GET {url}")
    for attempt in range(retries):
        try:
            async with session.get(url, timeout=aiohttp.ClientTimeout(total=30), ssl=False) as resp:
                return resp.status, host, path
        except Exception as e:
            logging.debug(f"Attempt {attempt + 1} failed for {url}: {e}")
            if attempt == retries - 1:
                return 0, host, path
            await asyncio.sleep(1)
async def check_https_bypass_async(session, host, node_name, nodeport, path="/", retries=2):
    """Test route using aiohttp with custom connector to bypass load balancer."""
    url = f"https://{host}{path}"
    logging.debug(f"Executing aiohttp bypass request: GET {url} via {node_name}:{nodeport}")
    # Create a custom connector that connects to the node directly
    connector = aiohttp.TCPConnector(ssl=False)
    # Override the connection resolution to connect to node_name:nodeport
    original_resolve_host = connector._resolve_host
    async def custom_resolve_host(host_info, port, traces=None):
        # If this is our target host, redirect to the node
        if host_info == host:
            # Return the node IP and nodeport instead
            return [{'hostname': node_name, 'host': node_name, 'port': nodeport, 'family': 2, 'proto': 6, 'flags': 0}]
        else:
            # Use original resolution for other hosts
            return await original_resolve_host(host_info, port, traces)
    connector._resolve_host = custom_resolve_host
    for attempt in range(retries):
        try:
            async with aiohttp.ClientSession(connector=connector) as bypass_session:
                # Set the Host header to the original hostname for proper routing
                headers = {'Host': host}
                async with bypass_session.get(url, headers=headers, timeout=aiohttp.ClientTimeout(total=30), ssl=False) as resp:
                    return resp.status, host, path
        except Exception as e:
            logging.debug(f"Bypass attempt {attempt + 1} failed for {url} via {node_name}:{nodeport}: {e}")
            if attempt == retries - 1:
                # Map common connection errors to special status codes
                if "Connection refused" in str(e) or "Cannot connect" in str(e):
                    return 7000, host, path  # Connection failed
                elif "timeout" in str(e).lower():
                    return 28000, host, path  # Timeout
                elif "ssl" in str(e).lower() or "certificate" in str(e).lower():
                    return 35000, host, path  # SSL error
                else:
                    return 0, host, path
            await asyncio.sleep(1)
def get_running_pod_node(v1_client: client.CoreV1Api, namespace: str) -> Optional[str]:
    """Get the node name of a running pod in the namespace."""
    try:
        pods = v1_client.list_namespaced_pod(
            namespace=namespace,
            field_selector="status.phase=Running",
            limit=1,
        )
        if pods.items:
            return pods.items[0].spec.node_name
        return None
    except ApiException:
        return None
def get_https_nodeport(v1_client: client.CoreV1Api, namespace: str) -> int:
    """Get the HTTPS NodePort from a service in the namespace."""
    try:
        services = v1_client.list_namespaced_service(namespace=namespace)
        for svc in services.items:
            if svc.spec.type == "NodePort" and svc.spec.ports:
                for port in svc.spec.ports:
                    if port.port == DEFAULT_HTTPS_PORT or (
                        port.name and "https" in port.name.lower()
                    ):
                        return port.node_port
    except ApiException:
        pass
    return DEFAULT_HTTPS_PORT
visa_route_status_counter = Counter(
    "visa_route_status_counter",
    "Count of occurrences of each HTTP status per run_id and namespace",
    ["run_id", "namespace", "status", "bypass_lb"]
)
async def run_checks_aiohttp_async(tasks, run_id, gauge, bypass_lb=False):
    """Run async aiohttp-based checks for standard or bypass validation."""
    v1_client = client.CoreV1Api()
    # Pre-fetch node and nodeport info for bypass mode
    node_info = {}
    if bypass_lb:
        for task in tasks:
            namespace = task["namespace"]
            if namespace not in node_info:
                node_name = get_running_pod_node(v1_client, namespace)
                if node_name:
                    nodeport = get_https_nodeport(v1_client, namespace)
                    node_info[namespace] = (node_name, nodeport)
                else:
                    node_info[namespace] = (None, None)
    # Create session for standard mode
    if not bypass_lb:
        connector = aiohttp.TCPConnector(limit=BATCH_SIZE, limit_per_host=5, ssl=False)
        timeout = aiohttp.ClientTimeout(total=30)
        session = aiohttp.ClientSession(connector=connector, timeout=timeout)
    else:
        session = None  # Will create individual sessions for bypass mode
    try:
        # Process tasks in batches for better performance
        for i in range(0, len(tasks), BATCH_SIZE):
            batch = tasks[i:i+BATCH_SIZE]
            # Create coroutines for the batch
            coros = []
            for task in batch:
                if bypass_lb:
                    node_name, nodeport = node_info.get(task["namespace"], (None, None))
                    if node_name:
                        coros.append(check_https_bypass_async(None, task["host"], node_name, nodeport, task["path"]))
                    else:
                        # No running pods - create a dummy coroutine that returns 0
                        async def dummy_coro():
                            return 0, task["host"], task["path"]
                        coros.append(dummy_coro())
                else:
                    coros.append(check_https_async(session, task["host"], task["path"]))
            # Execute batch concurrently
            for future in asyncio.as_completed(coros):
                status, host, path = await future
                # Find the task; since host and path uniquely identify it
                task = next(t for t in batch if t["host"] == host and t["path"] == path)
                gauge.labels(
                    task["namespace"],
                    str(host),
                    task["type"],

                    run_id,
                    "true" if bypass_lb else "false"  # bypass_lb label
                ).set(status)
                log_status_stdout(run_id, task["namespace"], host, status)
                visa_route_status_counter.labels(
                    run_id=run_id,
                    namespace=task["namespace"],
                    status=str(status),
                    bypass_lb="true" if bypass_lb else "false"
                ).inc()
            # Small pause between batches to avoid overwhelming the system
            await asyncio.sleep(1)
    finally:
        # Clean up session for standard mode
        if session:
            await session.close()
def get_namespaces(v1, namespace_arg):
    if namespace_arg:
        # Parse comma-separated namespaces
        ns_list = [ns.strip() for ns in namespace_arg.split(",") if ns.strip()]
        # Validate that each namespace exists
        valid_ns = []
        for ns in ns_list:
            try:
                v1.read_namespace(name=ns)
                valid_ns.append(ns)
            except Exception:
                logging.error(f"Namespace '{ns}' not found or not accessible!")
        if not valid_ns:
            logging.error("None of the provided namespaces are valid or accessible. Exiting.")
            sys.exit(1)
        return valid_ns
    else:
        namespaces = v1.list_namespace()
        envoy_namespaces = [ns.metadata.name for ns in namespaces.items if ns.metadata.name.startswith("envoy")]
        return envoy_namespaces
def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--namespace', help='Specify one or more namespaces (comma separated) for HTTP checks')
    parser.add_argument('--bypass-lb', action='store_true', help='Bypass load balancer by connecting directly to nodes')
    parser.add_argument('--debug', action='store_true', help='Enable debug logging')
    args = parser.parse_args()
    # Configure logging based on debug flag
    log_level = logging.DEBUG if args.debug else logging.WARNING
    logging.basicConfig(level=log_level, format="%(asctime)s %(levelname)s %(message)s", stream=sys.stderr)
    # Suppress verbose logging from underlying libraries even in debug mode
    logging.getLogger("urllib3").setLevel(logging.WARNING)
    logging.getLogger("kubernetes").setLevel(logging.WARNING)
    logging.getLogger("kubernetes.client").setLevel(logging.WARNING)
    logging.getLogger("kubernetes.client.rest").setLevel(logging.WARNING)
    if args.debug:
        logging.info(f"Using run_id: {RUN_ID}{' (generated)' if generated_run_id else ''}")
    load_kube_config()
    v1 = client.CoreV1Api()
    custom_api = client.CustomObjectsApi()
    # Update gauge to include bypass_lb label
    g = Gauge(
        "visa_route_status",
        "Route status for VirtualService",
        ["namespace", "hostname", "type", "run_id", "bypass_lb"]
    )
    # Get namespaces to check
    namespaces_to_check = get_namespaces(v1, args.namespace)
    # Prepare all tasks to check
    tasks = []
    for namespace in namespaces_to_check:
        vs_list = custom_api.list_namespaced_custom_object(
            group="networking.istio.io",
            version="v1beta1",
            namespace=namespace,
            plural="virtualservices"
        )
        for vs in vs_list.get("items", []):
            hosts = vs["spec"].get("hosts", [])
            # HTTP
            for http_route in vs["spec"].get("http", []):
                matches = http_route.get("match", [{}])
                for m in matches:
                    path = m.get("uri", {}).get("exact", "/")
                    for host in hosts:
                        tasks.append({
                            "namespace": namespace,
                            "host": host,
                            "type": "http",
                            "path": path
                        })
            # TCP
            for tcp_route in vs["spec"].get("tcp", []):
                matches = tcp_route.get("match", [{}])
                for m in matches:
                    for host in hosts:
                        tasks.append({
                            "namespace": namespace,
                            "host": host,
                            "type": "tcp",
                            "path": "/"
                        })
            # TLS
            for tls_route in vs["spec"].get("tls", []):
                matches = tls_route.get("match", [{}])
                for m in matches:
                    for host in hosts:
                        tasks.append({
                            "namespace": namespace,
                            "host": host,
                            "type": "tls",
                            "path": "/"
                        })
    # Run checks based on bypass_lb flag
    if args.bypass_lb:
        logging.info("Running route validation with load balancer bypass using aiohttp")
        asyncio.run(run_checks_aiohttp_async(tasks, RUN_ID, g, bypass_lb=True))
    else:
        logging.info("Running standard route validation using aiohttp")
        asyncio.run(run_checks_aiohttp_async(tasks, RUN_ID, g, bypass_lb=False))
    logging.info("Route validation completed for all namespaces.")
    # Expose metrics on /metrics
    start_http_server(METRICS_PORT)
    logging.info(f"Metrics server started on port {METRICS_PORT}")
    logging.info(f"Sleeping for {SCRAPE_WAIT_SECONDS} seconds to allow Prometheus to scrape before exiting...")
    time.sleep(SCRAPE_WAIT_SECONDS)
    logging.info("Exiting.")
if __name__ == "__main__":
    import warnings
    warnings.filterwarnings("ignore", category=UserWarning, module='aiohttp')
    main()
