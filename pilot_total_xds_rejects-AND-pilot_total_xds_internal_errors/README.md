# pilot_total_xds_rejects and pilot_total_xds_internal_errors not seen in istiod 15014 /metrics endpoint
There was a concern from the customer that 
pilot_total_xds_rejects and pilot_total_xds_internal_errors 
not seen in istiod 15014 /metrics endpoint

These metrics only appear after they increment at least once. If istiod hasn’t seen any xDS internal errors or NACK/rejects since it started, Prometheus won’t expose pilot_total_xds_internal_errors or pilot_total_xds_rejects at /metrics.

## An envoy_filter to expose this

```
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: trigger-nack-type-mismatch
  namespace: httpbin
spec:
  configPatches:
  - applyTo: HTTP_FILTER
    match:
      context: SIDECAR_OUTBOUND
      listener:
        filterChain:
          filter:
            name: envoy.filters.network.http_connection_manager
            subFilter:
              name: envoy.filters.http.router
    patch:
      operation: INSERT_BEFORE
      value:
        name: envoy.filters.http.router
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
          stat_prefix: wrong
```

This will cause errors like
```
10.96.77.85_18000: Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: 'envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy'
10.96.42.33_3000: Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: 'envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy'
	thread=16
2025-09-22T12:21:02.615975Z	info	xdsproxy	connected to delta upstream XDS server: istiod-1-25-3.istio-system.svc:15012	id=12
2025-09-22T12:21:02.668581Z	warning	envoy config external/envoy/source/extensions/config_subscription/grpc/delta_subscription_state.cc:296	delta config for type.googleapis.com/envoy.config.listener.v3.Listener rejected: Error adding/updating listener(s) 0.0.0.0_9000: Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: 'envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy'
0.0.0.0_80: Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: 'envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy'
0.0.0.0_9090: Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: 'envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy'
10.96.164.64_9443: Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: 'envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy'
10.96.77.85_18000: Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: 'envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy'
0.0.0.0_18000: Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: 'envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy'
10.96.164.64_18002: Didn't find a registered implementation for 'envoy.filters.http.router' with type URL: 'envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy'
```

And increment pilot_total_xds_rejects

