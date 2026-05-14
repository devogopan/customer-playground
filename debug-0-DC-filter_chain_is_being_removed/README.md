
## Problem Summary

Customer is seeing 503 in the caller and 0 DC at server. Something like this

Client - "GET /abcd-service/xyz/persistLMWSData HTTP/1.1" 503 UC upstream_reset_before_response_started{connection_termination}
Server - "GET /abcd-service/xyz/persistZOSLMWSData HTTP/1.1" 0 DC downstream_local_disconnect(filter_chain_is_being_removed)

Server application gets the request eventhough there are are errors in proxy and it starts the function & completes it as well.

## Explanation

0 DC downstream_local_disconnect(filter_chain_is_being_removed) means when the request was in-flight filters got removed and envoy disconnected downstream client because listener filter was also removed. Since the job is completing I am sure request is relayed to server application from envoy.|

503 UC upstream_reset_before_response_started{connection_termination} means upstream server terminated underlying tcp connection

Filters normally get removed as above in step 1 when a full push happens from istiod. I can see a full push happened at 00:14:35 with reason unknown

"Apr 28, 2026 @ 00:14:35.966","2026-04-28T04:14:35.966606Z info ads Push debounce stable[5251] 1 for reason unknown:1: 100.237856ms since last change, 100.237756ms since last push, full=true"

Once a full push happened to a istio-proxy it has to remove the filters and build them again based on the new config it has received from istiod. This particular request which sees a “0 DC” was inflight at that time. And since listener filters were removed exactly at that time istio-proxy drains the connection from the clients at that time.

## RCA

Now what could be the trigger for this fullpush is below line I believe. 
"Apr 28, 2026 @ 00:14:35.860","2026-04-28T04:14:35.859936Z info model Updated cached JWT public key from ""https://secure.abc-corp.com/ext/authtoken/JWKS"""

You can note the reason unknown:1: for istiod full mentioned in point no. 3. in fact if a full push happens for Updated cached JWT public key the reason comes as unknown.  Actually, looking at the code, the push function is only called if the JWKS at the jwksUri secure.fhlmc.com/ext/authtoken/JWKS has changed. So could you answer me 

## To reproduce this.

I am using Tetrate TSB. A service mesh solution based on Istio. So there can be slight differences but I can explain that when needed.

1. There is a slow-server(**slow-server.yaml**) which can respond to a request with delay 
```
curl -s -o /dev/null -w "HTTP status: %{http_code}\nTotal time: %{time_total}s\n" http://slow-service.httpbin:8080/delay/75 &
````
This tells we app will respond with a delay of 75s

2. A JWKS server(**jwks-server.yaml**) which can provide tokens for requestAuthentication. This has following endpoints 
GET - /.well-known/jwks.json (for istiod to fetch jwks)
GET - /status (will tell you which key is used)
POST - /rotate - (will rotate keys defined in **jwks-data.yaml**)


**jwks-data.yaml** defines two different jwks sets.

3. A requestAuthentication (**reqauth.yaml**)

4. Only for TSB, trafficSetting(**trafficSetting.yaml**) to disable retries on 503
This need to be applied using "tctl"

5. For Istio, a VS to disable retries (**vs.yaml**)



