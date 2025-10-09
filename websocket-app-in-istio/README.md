# Testing the Python WebSocket Echo Server via Istio Gateway

## 1) Build and push the image
Make sure you are in the directory with app.py, requirements.txt, and Dockerfile

```bash

docker build --platform=linux/amd64 --load -t websocket-echo:1.0 .
docker image ls | grep websocket
docker tag  websocket-echo:1.0 devogopan/websocket-echo:1.0
docker push devogopan/websocket-echo:1.0

```

## 2) Deploy to Kubernetes with Istio sidecar injection
Make sure you have Istio installed. I have Istio with revision 1-24-2 installed

```bash
kubectl apply -f k8-manifest.yaml.yaml
kubectl apply -f istio-manifest.yaml.yaml

# Wait for readiness
kubectl -n websocket-test rollout status deploy/py-ws-echo
```

## 3) Get ingress address

Get ingressgateway service IP/name. I am doing it in Kind cluster. So it is a name.

```bash
export INGRESS="172.18.255.200"
```

## 4) Test HTTP health

```bash
curl -H "Host: ws.example.local" http://$INGRESS/health
```

## 5) Test WebSocket with wscat (recommended)

```bash
npm i -g wscat
wscat -c "ws://$INGRESS/ws" --header "Host: ws.example.local"
# Then type messages; you should see them echoed back with "echo: ..."
```

## 6) Test WebSocket handshake with curl
This should show http code 101

```bash
curl -i -N \
  -H "Connection: Upgrade" \
  -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" \
  -H "Sec-WebSocket-Key: x3JJHMbDL1EzLkh9GBhXDw==" \
  -H "Host: ws.example.local" \
  http://$INGRESS/ws
```

## Notes
- Adjust host `ws.example.local` or add a DNS entry or use /etc/hosts mapping for browser tests.

