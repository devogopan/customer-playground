# Send traces and ALS logs to Otel collector
This will create a Kind cluster, Install Istio, Setup Otel and Configure Istio to send traces and ALS to Otel collector

## 1. Create a Kind cluster
From Istio Github repo, run the following command to create a Kind cluster.

    samples/kind-lb/setupkind.sh --cluster-name istio-grafana --ip-space 254
    export KUBECONFIG=$PWD/cluster-kubeconfig.yaml
    kind export kubeconfig --name istio-grafana
    kubectl get pods -A

Check that you are seeing pods listed.
## 2. Install Istio
Install Istio base, istiod and gateway using helm

    kubectl create ns istio-system 

    helm install istio-base istio/base \
      --namespace istio-system \
      --version 1.22.5 \
      --wait
    
    helm install istiod-1-22-5 istio/istiod \
      --namespace istio-system \
      --version 1.22.5 \
      --set revision=1-22-5 \
      --set pilot.autoscaleEnabled=true \
      -f values.yaml \
      --wait!
      
    helm install istio-gw-1-22-5 istio/gateway \ 
    --namespace istio-system --version 1.22.5 \ 
    --set revision=1-22-5 --set meshConfig.accessLogFile="/dev/stdout" \ 
    --wait

Check that istiod pods and istio-gw pod is running.
meshConfig for sending Traces and ALS to Otel is added in values file.

## 3. Install bookinfo

    kubectl create ns bookinfo
    kubectl label ns bookinfo istio.io/rev=1-22-5
    kubectl apply -n bookinfo -f https://raw.githubusercontent.com/istio/istio/release-1.22/samples/bookinfo/platform/kube/bookinfo.yaml
    
    kubectl apply -f bookinfo-gw-vs.yaml

   For sending traffic to bookinfo, take the LB IP of istio-gw service running in istio-system and curl that

    kubectl get svc -n istio-system
    curl -v -s http://<lb-ip-of-the-istio-gw-svc>/productpage

## 4. Install Otel collector
First install certmanager and wait for certmanager to come up

    kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.18.2/cert-manager.yaml

Now install opentelemetry operator

    kubectl apply -f https://github.com/open-telemetry/opentelemetry-operator/releases/latest/download/opentelemetry-operator.yaml
Now install *OpenTelemetryCollector* CR to install Open Telemetry.

    kubectl create ns observability
    kubectl apply -f otel-config.yaml

## 5. Install Istio Telemetry and check Otel collector logs

    kubectl apply -f telemetry.yaml

Now you can curl and check the logs of Otel collector pod in "observability" to see traces and ALS logs.
