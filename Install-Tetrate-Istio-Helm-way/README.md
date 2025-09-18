## 1. Create a Kind cluster

From Istio Github repo, run the following command to create a Kind cluster.

```
samples/kind-lb/setupkind.sh --cluster-name istio-grafana --ip-space 254
export KUBECONFIG=$PWD/cluster-kubeconfig.yaml
kind export kubeconfig --name istio-grafana
kubectl get pods -A

```

Check that you are seeing pods listed.

## 2. Install Tetrate FIPS Istio Distro

    export VERSION="1.24.3+tetrate4"
    export TAG="1.24.3-tetratefips4"
    
    export TIS_USER="your-username-for-tid"
    export TIS_PASS="your-password-for-tid"
    
    kubectl create namespace istio-system
    
    kubectl create secret docker-registry tetrate-fips-creds \
        --docker-server="fips-containers.istio.tetratelabs.com" \
        --docker-username=${TIS_USER} \
        --docker-password=${TIS_PASS} \
        --docker-email="gopakumar.n@tetrate.io" \
        -n istio-system
    
    
    helm upgrade --install istio-base tetratelabs/base -n istio-system \
        --set global.tag=${TAG} \
        --set global.hub="fips-containers.istio.tetratelabs.com" \
        --set "global.imagePullSecrets[0]=tetrate-fips-creds" \
        --version ${VERSION}
    
    helm upgrade --install istiod tetratelabs/istiod -n istio-system \
        --set global.tag=${TAG} \
        --set global.hub="fips-containers.istio.tetratelabs.com" \
        --set "global.imagePullSecrets[0]=tetrate-fips-creds" \
        --set "pilot.env.COMPLIANCE_POLICY=fips-140-2" \
        --version ${VERSION} \
        --set values.pilot.certProvider=custom \
        --wait

Check the pods are up.
