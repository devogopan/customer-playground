package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Route types.
const (
	typeHTTP = "http"
	typeTCP  = "tcp"
	typeTLS  = "tls"
)

// Reasons a route is not checked. Counted per namespace in the summary and
// in visa_route_skipped_total.
const (
	skipMeshOnly = "mesh_only"     // VirtualService not bound to any gateway (counted per VirtualService)
	skipWildcard = "wildcard_host" // host contains '*' (counted per host)
	skipRegex    = "regex_match"   // uri.regex match: no concrete path to request (counted per match)
)

const defaultGatewayPort = 443

// VirtualService API versions to try, newest first. Older Istio releases do
// not serve v1.
var vsVersions = []string{"v1", "v1beta1"}

const listPageSize = 500

type task struct {
	Namespace string
	Host      string
	Type      string // http, tcp or tls
	Path      string // http only
	Port      int    // gateway port
}

type discovery struct {
	tasks []task
	// skipped[namespace][reason] = count
	skipped map[string]map[string]int
	// namespaces whose VirtualServices could not be listed
	failedNamespaces []string
}

func (d *discovery) skip(namespace, reason string, n int) {
	if n == 0 {
		return
	}
	if d.skipped[namespace] == nil {
		d.skipped[namespace] = map[string]int{}
	}
	d.skipped[namespace][reason] += n
}

func getNamespaces(ctx context.Context, cs kubernetes.Interface, namespaceArg, prefix string) ([]string, error) {
	if namespaceArg != "" {
		var valid []string
		for _, ns := range strings.Split(namespaceArg, ",") {
			ns = strings.TrimSpace(ns)
			if ns == "" {
				continue
			}
			if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
				slog.Error(fmt.Sprintf("Namespace '%s' not found or not accessible!", ns), "err", err)
				continue
			}
			valid = append(valid, ns)
		}
		if len(valid) == 0 {
			return nil, fmt.Errorf("none of the provided namespaces are valid or accessible")
		}
		return valid, nil
	}

	list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	var matched []string
	for _, ns := range list.Items {
		if strings.HasPrefix(ns.Name, prefix) {
			matched = append(matched, ns.Name)
		}
	}
	return matched, nil
}

// listVirtualServices lists all VirtualServices in a namespace, page by page,
// falling back to older API versions when a version is not served.
func listVirtualServices(ctx context.Context, dyn dynamic.Interface, namespace string) ([]unstructured.Unstructured, error) {
	var lastErr error
	for _, version := range vsVersions {
		gvr := schema.GroupVersionResource{Group: "networking.istio.io", Version: version, Resource: "virtualservices"}
		var items []unstructured.Unstructured
		opts := metav1.ListOptions{Limit: listPageSize}
		for {
			list, err := dyn.Resource(gvr).Namespace(namespace).List(ctx, opts)
			if err != nil {
				lastErr = err
				break
			}
			items = append(items, list.Items...)
			if list.GetContinue() == "" {
				return items, nil
			}
			opts.Continue = list.GetContinue()
		}
		if !apierrors.IsNotFound(lastErr) {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// buildTasks creates one check per host per route match, in VirtualService
// order (http, then tcp, then tls routes), without duplicates.
func buildTasks(ctx context.Context, dyn dynamic.Interface, namespaces []string) *discovery {
	d := &discovery{skipped: map[string]map[string]int{}}
	seen := map[task]bool{}
	add := func(t task) {
		if !seen[t] {
			seen[t] = true
			d.tasks = append(d.tasks, t)
		}
	}

	for _, ns := range namespaces {
		items, err := listVirtualServices(ctx, dyn, ns)
		if err != nil {
			slog.Error("Listing VirtualServices failed, skipping namespace", "namespace", ns, "err", err)
			d.failedNamespaces = append(d.failedNamespaces, ns)
			continue
		}
		for _, vs := range items {
			spec, _ := vs.Object["spec"].(map[string]interface{})
			if !boundToGateway(spec["gateways"]) {
				d.skip(ns, skipMeshOnly, 1)
				continue
			}

			var hosts []string
			for _, h := range toStrings(spec["hosts"]) {
				if strings.Contains(h, "*") {
					d.skip(ns, skipWildcard, 1)
					continue
				}
				hosts = append(hosts, h)
			}

			for _, route := range toSlice(spec[typeHTTP]) {
				for _, match := range routeMatches(route) {
					path, ok := httpPath(match)
					if !ok {
						d.skip(ns, skipRegex, 1)
						continue
					}
					port := matchPort(match)
					for _, h := range hosts {
						add(task{Namespace: ns, Host: h, Type: typeHTTP, Path: path, Port: port})
					}
				}
			}
			for _, routeType := range []string{typeTCP, typeTLS} {
				for _, route := range toSlice(spec[routeType]) {
					for _, match := range routeMatches(route) {
						port := matchPort(match)
						for _, h := range hosts {
							add(task{Namespace: ns, Host: h, Type: routeType, Port: port})
						}
					}
				}
			}
		}
	}
	return d
}

// boundToGateway reports whether the VirtualService applies to at least one
// gateway. With no gateways it only applies to sidecars ("mesh").
func boundToGateway(gateways interface{}) bool {
	for _, g := range toStrings(gateways) {
		if g != "mesh" {
			return true
		}
	}
	return false
}

// routeMatches returns the route's match list, or a single empty match when
// the route has none.
func routeMatches(route interface{}) []interface{} {
	r, _ := route.(map[string]interface{})
	m := toSlice(r["match"])
	if len(m) == 0 {
		return []interface{}{map[string]interface{}{}}
	}
	return m
}

// httpPath returns the path to request for an http match: uri.exact or
// uri.prefix, "/" when the match has no uri, and false for uri.regex.
func httpPath(match interface{}) (string, bool) {
	m, _ := match.(map[string]interface{})
	uri, _ := m["uri"].(map[string]interface{})
	if exact, _ := uri["exact"].(string); exact != "" {
		return exact, true
	}
	if prefix, _ := uri["prefix"].(string); prefix != "" {
		return prefix, true
	}
	if _, isRegex := uri["regex"]; isRegex {
		return "", false
	}
	return "/", true
}

// matchPort returns match.port, or 443 when unset.
func matchPort(match interface{}) int {
	m, _ := match.(map[string]interface{})
	switch p := m["port"].(type) {
	case int64:
		return int(p)
	case float64:
		return int(p)
	case int:
		return p
	}
	return defaultGatewayPort
}

func toSlice(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}

func toStrings(v interface{}) []string {
	var out []string
	for _, item := range toSlice(v) {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
