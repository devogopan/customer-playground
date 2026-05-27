1. Install Istio GH example sleep pod in sleep namespace.
2. From sleep pod's shell, execute 

```shell
curl -s -o /dev/null -w "HTTP status: %{http_code}\nTotal time: %{time_total}s\n" http://slow-service.httpbin:8080/delay/75 &

CURL_PID=$!
echo "Curl PID: $CURL_PID"
echo "Request started at: $(date -u)"

sleep 1

echo "=== Rotating JWKS at $(date -u) ==="


curl -s -X POST http://jwks-server.httpbin/rotate -v
curl -s http://jwks-server.httpbin/status -X GET

```
3. From sleep pod trigger an event to trigger re-fetch JWKS
```shell
echo "=== Cycling RequestAuthentication to trigger re-fetch at $(date -u) ==="

kubectl delete -f req-auth.yaml
sleep 1
kubectl apply -f req-auth.yaml
```
4. Wait for 75s for curl executed at step 2
5. You should see in sleep pod
```
[2026-05-27T15:02:22.469Z] "GET /delay/75 HTTP/1.1" 503 UC upstream_reset_before_response_started{connection_termination} - "-" 0 95 68713 - "-" "curl/8.16.0" "8ecbb9ca-e733-4ecf-b646-511a32e0128e" "slow-service.httpbin:8080" "10.244.0.50:8080" outbound|8080||slow-service.httpbin.svc.cluster.local 10.244.0.48:33540 10.96.209.225:8080 10.244.0.48:38732 - default

```
5. You should see in slow-server pod
```
[2026-05-27T15:02:22.472Z] "GET /delay/75 HTTP/1.1" 0 DC downstream_local_disconnect(filter_chain_is_being_removed) - "-" 0 0 68710 - "-" "curl/8.16.0" "8ecbb9ca-e733-4ecf-b646-511a32e0128e" "slow-service.httpbin:8080" "10.244.0.50:8080" inbound|8080|| 127.0.0.6:41209 10.244.0.50:8080 10.244.0.48:33540 outbound_.8080_._.slow-service.httpbin.svc.cluster.local default
```
