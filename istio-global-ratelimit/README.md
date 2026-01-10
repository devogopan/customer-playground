# Istio Global Rate Limiting

This directory contains manifests to setup and test the Istio global rate limiting

this is setting up outbound ratelimiting from sleep pod.

## Files
- `namespace.yaml` - Creates `ratelimit-demo` namespace with Istio revision label
- `httpbin.yaml` - Test application (httpbin) with proxy config to disable delta XDS
- `sidecar.yaml` - Sidecar resource to scope egress traffic
- `redis-ratelimit.yaml` - Redis and rate limit service deployment
- `envoyfilter-ratelimit-filter.yaml` - EnvoyFilter to inject rate limit HTTP filter
- `envoyfilter-ratelimit-routes.yaml` - **Working** EnvoyFilter with specific virtual host match
- `sleep.yaml` - Test pod for making requests
- `working.json` - Example working proxy config dump

## Deployment

### Prerequisites

- Istio 1.24.2+ installed
- `istio-system` namespace exists
- kubectl access to cluster

### Steps

1. **Deploy namespace:**
   kubectl apply -f namespace.yaml
   2. **Deploy Redis and rate limit service:**
   kubectl apply -f redis-ratelimit.yaml
   3. **Deploy httpbin and sidecar:**
   
   kubectl apply -f httpbin.yaml
   kubectl apply -f sidecar.yaml
   4. **Deploy EnvoyFilters:**h
   kubectl apply -f envoyfilter-ratelimit-filter.yaml
   kubectl apply -f envoyfilter-ratelimit-routes.yaml  # Working version
   # OR
   kubectl apply -f envoyfilter-ratelimit-routes-problem.yaml  # Problematic version
   5. **Deploy sleep pod for testing:**
   kubectl apply -f sleep.yaml
   6. **Wait for pods to be ready:**
  kubectl wait --for=condition=ready pod -l app=ratelimit -n istio-system --timeout=60s
   kubectl wait --for=condition=ready pod -l app=redis -n istio-system --timeout=60s
   kubectl wait --for=condition=ready pod -l app=httpbin -n ratelimit-demo --timeout=60s
   kubectl wait --for=condition=ready pod -l app=sleep -n ratelimit-demo --timeout=60s
   ## Testing

### Test Rate Limiting

Make 25 requests (should see 429 after 20):

SLEEP_POD=$(kubectl get pod -n ratelimit-demo -l app=sleep -o jsonpath='{.items[0].metadata.name}')

kubectl exec -it -n ratelimit-demo $SLEEP_POD -- sh
#counter=1; while true; do curl -s -o /dev/null -w "Request $counter: %{http_code}\n" http://httpbin.ratelimit-demo.svc.cluster.local; sleep 0.1; counter=$((counter + 1)) ; done ;



