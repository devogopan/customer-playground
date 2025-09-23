# Traffic mirroring from istio GW to another GW (may be in another cluster)
This will create couple of Kind clusters, Install Istio, Setup mirroring of traffic

## 1. Create Kind cluster
From Istio Github repo, run the following command to create a Kind cluster.

    samples/kind-lb/setupkind.sh --cluster-name istio-1 --ip-space 254
    export KUBECONFIG=$PWD/cluster1-kubeconfig.yaml
    kind export kubeconfig --name istio-1
    kubectl get pods -A

    samples/kind-lb/setupkind.sh --cluster-name istio-2 --ip-space 255
    export KUBECONFIG=$PWD/cluster2-kubeconfig.yaml
    kind export kubeconfig --name istio-2
    kubectl get pods -A

Check that you are seeing pods listed.

Another important step here is to check that LB address is actually different and still they can communicate.

## 2. Install Istio
Install Istio base, istiod and gateway using helm in both the clusters.

    kubectl create ns istio-system 

    helm install istio-base istio/base \
      --namespace istio-system \
      --version 1.26.3 \
      --wait
    
    helm install istiod-1-26-3 istio/istiod \
      --namespace istio-system \
      --version 1.26.3 \
      --set revision=1-26-3 \
      --set pilot.autoscaleEnabled=true \
      --set meshConfig.accessLogFile="/dev/stdout" \
      --wait
      
    helm install istio-gw-1-26-3 istio/gateway \
      --namespace istio-system --version 1.26.3 \
      --set revision=1-26-3 --set meshConfig.accessLogFile="/dev/stdout" \
      --wait

Check that istiod pods and istio-gw pod is running.

## 3. Install httpbin in first cluster

    kubectl create ns httpbin
    kubectl label ns httpbin istio.io/rev=1-26-3
    kubectl apply -n httpbin -f https://raw.githubusercontent.com/istio/istio/master/samples/httpbin/httpbin.yaml
    
    kubectl apply -f httpbin-gw.yaml
    kubectl apply -f httpbin-vs.yaml
    kubectl apply -f httpbin-se.yaml
    kubectl apply -f httpbin-dr.yaml


For sending traffic to bookinfo, take the LB IP of istio-gw service running in istio-system and curl that

    kubectl get svc -n istio-system
    curl -v -k -s http://pod1-apigw.rel.anfaqa.com/ip --connect-to pod1-apigw.rel.anfaqa.com:80:<LB-IP>:80

## 4. Install httpbin in second cluster
Actually nothing needed to be installed in second cluster. We just need to install GW install which is given in step 2 with an LB IP reachable from first cluster.

## 5. Check the traffic is mirrored
As given in step 3, curl to GW host. You should get a 200 and IP address of the client.
Also in the logs of second cluster's GW you should see a log like this.


    [2025-09-22T09:37:33.814Z] "GET /ip HTTP/1.1" 404 NR route_not_found - "-" 0 0 0 - "10.244.1.1,10.244.1.27,172.18.0.8" "curl/8.7.1" "fe4105fd-c842-44a8-8799-3d4c9f793786" "pod1-apigw.rel.anfaqa.com-shadow" "-" - - 10.244.1.9:80 172.18.0.8:37340 - -

