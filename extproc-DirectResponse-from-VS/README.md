## 1. Scenario

DirectResponse from VirtualService to be processed by the extProc filter for response header manipulation.
After upgrading Istio from 1.24.x to 1.26.x, this is broken. This change breaks CORS header injection in OPTIONS preflight requests handled via DirectResponse.

I have used 1.24.x 

## 2. Steps to reproduce

In a kind cluster deploy istiod and istio gateway with version 1.24.x.
I have created a golang microservice to work as extproc. Source code, dockerfile and deployment yaml is given with this.

1. Create namespace called httpbin and label it with "istio.io/rev: 1.24.3"
2. Install httpbin app in the namespace httpbin
3. Install istio GW in httpbin namespace 
```
helm install istio-ingressgateway istio/gateway --namespace httpbin --version 1.22.5 --set revision=1-22-5 --wait
```
4. Install the httpbin-gw.yaml and httpbin-vs.yaml
5. Install the localresponse-vs.yaml
6. Install the extproc.yaml
7. Curl commands
```
curl -X OPTIONS -v  -H "accept: application/json"  "http://pod1-apigw.rel.anfaqa.com/response-headers?freeform=hello"

curl -X GET -v  -H "accept: application/json"  "http://pod1-apigw.rel.anfaqa.com/response-headers?freeform=hello"

```
For curl to work, edit your laptop's /etc/hosts file to add "172.18.254.200 pod1-apigw.rel.anfaqa.com". IP is the LB IP of the ingressgateway in httpbin namespace.

8. Verify output 

First curl will have a response header "x-extproc-hello: Hello from ext_proc" and no output. This response is coming straight from ingress gateway and on the response path extproc adds this header. you can verify this by checking the logs of the ingress gateway. 

```
[2025-09-23T09:19:48.950Z] "OPTIONS /response-headers?freeform=hello HTTP/1.1" 200 - direct_response - "-" 0 0 3 - "10.244.1.1" "curl/8.7.1" "a43e29a7-2512-4764-a9b9-e06b4ea5e3ca" "pod1-apigw.rel.anfaqa.com" "-" - - 10.244.1.38:80 10.244.1.1:63991 - option-call
```

Second curl will be sent to httpbin app and you will get a response like below and a response header "x-extproc-hello: Hello from ext_proc" 

```
{
  "freeform": [
    "hello"
  ]
}
```
In the ingress gateway you can check the logs

```
[2025-09-23T09:25:37.161Z] "GET /response-headers?freeform=hello HTTP/1.1" 200 - via_upstream - "-" 0 36 4 2 "10.244.1.1" "curl/8.7.1" "fa877807-5b59-4ed0-8386-b10f0c7249bc" "pod1-apigw.rel.anfaqa.com" "10.244.2.26:8080" outbound|8000||httpbin.httpbin.svc.cluster.local 10.244.1.38:38010 10.244.1.38:80 10.244.1.1:50935 - -
```

## 3. Build the docker image
docker build --platform=linux/amd64 --load -t extproc-server:v1.6 .


## 4. This feature is broken after 1.24.x

The change is due to Envoy’s updated handling of local replies, which now bypasses most HTTP filters for DirectResponse. This results in failed CORS checks on the client side.

The root cause is a CVE‑related change in Envoy 1.34 (bundled with Istio 1.26) that skips extProc processing on local replies. The issue was resolved by updating the extProc filter to add the required headers during request processing, and the customer applied a workaround in their application to inject the headers. No further action is required.

This is a planned change in envoy code for CVE fixes as local reply with ext proc has some edge cases resulted in crash. 
[Envoy crashes when HTTP ext_proc processes local replies](https://github.com/envoyproxy/envoy/security/advisories/GHSA-cf3q-gqg7-3fm9)

This can be reverted by using the flag mentioned in PR
[\[IMPORTANT\] CVE fix by phlax · Pull Request #38818 · envoyproxy/envoy](https://github.com/envoyproxy/envoy/pull/38818/files)

May be we should be use a dummy svc and workload which will just have a simple response to the “OPTIONS” call and this response will get redirected to ext_proc and come back with correct headers set there.

We may could add an internal listener or fake listener which will send directResponse directly. And the original main listener will proxy the request to the fake listener, the response from fake listener will be treated as upstream response. Then at least we needn’t a dummy svc or something.