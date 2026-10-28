package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// gatewayTarget is where --bypass-lb connections for one namespace go: the
// node (hostIP) of a ready pod behind the gateway Service, on that Service's
// NodePorts. Using a pod selected by the Service keeps this correct with
// externalTrafficPolicy: Local, where only nodes running a gateway pod accept
// NodePort traffic.
type gatewayTarget struct {
	Service   string
	Pod       string
	HostIP    string
	NodePorts map[int]int // service port -> nodePort
}

func (g *gatewayTarget) String() string {
	return fmt.Sprintf("service=%s pod=%s node=%s nodePorts=%v", g.Service, g.Pod, g.HostIP, g.NodePorts)
}

// findGatewayTarget picks the gateway Service in the namespace (NodePort or
// LoadBalancer, with an HTTPS port that has a nodePort) and a ready pod it
// selects.
func findGatewayTarget(ctx context.Context, cs kubernetes.Interface, namespace string) (*gatewayTarget, error) {
	services, err := cs.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	var candidates []corev1.Service
	for _, svc := range services.Items {
		if (svc.Spec.Type == corev1.ServiceTypeNodePort || svc.Spec.Type == corev1.ServiceTypeLoadBalancer) &&
			httpsNodePort(svc) != 0 {
			candidates = append(candidates, svc)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no NodePort/LoadBalancer service with an HTTPS nodePort")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	svc := candidates[0]
	if len(candidates) > 1 {
		var names []string
		for _, c := range candidates {
			names = append(names, c.Name)
		}
		slog.Warn("Multiple gateway services found, using the first", "namespace", namespace,
			"services", strings.Join(names, ","), "using", svc.Name)
	}
	if len(svc.Spec.Selector) == 0 {
		return nil, fmt.Errorf("service %s has no pod selector", svc.Name)
	}

	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods for service %s: %w", svc.Name, err)
	}
	var ready []corev1.Pod
	for _, pod := range pods.Items {
		if podReady(pod) {
			ready = append(ready, pod)
		}
	}
	if len(ready) == 0 {
		return nil, fmt.Errorf("no ready pods match service %s selector %s (%d pods found)",
			svc.Name, labels.SelectorFromSet(svc.Spec.Selector), len(pods.Items))
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })

	nodePorts := map[int]int{}
	for _, p := range svc.Spec.Ports {
		if p.NodePort != 0 {
			nodePorts[int(p.Port)] = int(p.NodePort)
		}
	}
	return &gatewayTarget{
		Service:   svc.Name,
		Pod:       ready[0].Name,
		HostIP:    ready[0].Status.HostIP,
		NodePorts: nodePorts,
	}, nil
}

// httpsNodePort returns the nodePort of the service's HTTPS port (port 443 or
// a port named *https*), or 0.
func httpsNodePort(svc corev1.Service) int {
	for _, p := range svc.Spec.Ports {
		if p.NodePort != 0 && (p.Port == defaultGatewayPort || strings.Contains(strings.ToLower(p.Name), "https")) {
			return int(p.NodePort)
		}
	}
	return 0
}

func podReady(pod corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.Status.HostIP == "" || pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
