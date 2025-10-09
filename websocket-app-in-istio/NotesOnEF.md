# Rationale

## Websocket not working with EF customer had
Customer had a problem where GW pod was sending a RST immediately after 401 was seen from upstream. 
Downstream device not prepared for this and it was still transmitting data. So it took this RST as 503
and sent it to it's downstream. More details here.
https://github.com/envoyproxy/envoy/issues/13781

So they used and EF like below. 

```bash

---
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: request-size-limit
  namespace: istio-system
spec:
  workloadSelector:
    labels:
      app: istio-gw-1-24-2
  configPatches:
    - applyTo: HTTP_FILTER
      match:
        context: GATEWAY
        listener:
          filterChain:
            filter:
              name: "envoy.filters.network.http_connection_manager"
      patch:
        operation: INSERT_BEFORE
        value:
          name: envoy.filters.http.buffer
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.filters.http.buffer.v3.Buffer
            max_request_bytes: 2097152 #2MB
---

```

But it caused problems in websocket as given in
https://github.com/envoyproxy/envoy/issues/19645

So they moved to another EF now

```bash

---
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: max-request-size
  namespace: istio-system
spec:
  workloadSelector:
    labels:
      app: istio-gw-1-24-2
  configPatches:
  - applyTo: HTTP_FILTER
    match:
      context: GATEWAY
      listener:
        filterChain:
          filter:
            name: envoy.filters.network.http_connection_manager
            subFilter:
              name: "envoy.filters.http.router"
    patch:
      operation: INSERT_BEFORE
      value:
        name: with-matcher
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.common.matching.v3.ExtensionWithMatcher
          extension_config:
            name: envoy.filters.http.buffer
            typed_config:
              '@type': type.googleapis.com/envoy.extensions.filters.http.buffer.v3.Buffer
              max_request_bytes: 52428800
          xds_matcher:
            matcher_tree:
              input:
                name: request-headers
                typed_config:
                  "@type": type.googleapis.com/envoy.type.matcher.v3.HttpRequestHeaderMatchInput
                  header_name: Upgrade
              exact_match_map:
                map:
                  websocket:
                    action:
                      name: skip
                      typed_config:
                        "@type": type.googleapis.com/envoy.extensions.filters.common.matcher.action.v3.SkipFilter
---
```
