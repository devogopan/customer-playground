// Command validate-routes checks every route exposed through Istio gateways.
//
// It lists VirtualServices in the envoy* namespaces (or --namespace), checks
// every host x route match on a pool of --concurrency workers, prints
// "run_id,namespace,hostname,status" per check to stdout and exposes the
// results on :8000/metrics before exiting. See README.md for flags and
// status codes.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	metricsPort        = 8000
	defaultConcurrency = 50
)

type options struct {
	namespace       string
	namespacePrefix string
	bypassLB        bool
	debug           bool
	concurrency     int
	timeout         time.Duration
	retries         int
	followRedirects bool
	verifyTLS       bool
	csvExtended     bool
	failOnError     bool
}

type metrics struct {
	status  *prometheus.GaugeVec
	counter *prometheus.CounterVec
	skipped *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		status: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "visa_route_status",
			Help: "Route status for VirtualService",
		}, []string{"namespace", "hostname", "type", "path", "run_id", "bypass_lb"}),
		// prometheus_client (Python) appends _total to counters; keep the same exported name.
		counter: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "visa_route_status_counter_total",
			Help: "Count of occurrences of each HTTP status per run_id and namespace",
		}, []string{"run_id", "namespace", "status", "bypass_lb"}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "visa_route_skipped_total",
			Help: "Routes not checked, by reason (mesh_only, wildcard_host, regex_match)",
		}, []string{"run_id", "namespace", "reason"}),
	}
	reg.MustRegister(m.status, m.counter, m.skipped)
	return m
}

func main() {
	var o options
	flag.StringVar(&o.namespace, "namespace", "", "Specify one or more namespaces (comma separated) for HTTP checks")
	flag.StringVar(&o.namespacePrefix, "namespace-prefix", "envoy", "Namespace name prefix used when --namespace is not set")
	flag.BoolVar(&o.bypassLB, "bypass-lb", false, "Bypass load balancer by connecting directly to the node of a ready gateway pod")
	flag.BoolVar(&o.debug, "debug", false, "Enable debug logging")
	flag.IntVar(&o.concurrency, "concurrency", defaultConcurrency, "Number of simultaneous checks")
	flag.DurationVar(&o.timeout, "timeout", 30*time.Second, "Timeout per attempt")
	flag.IntVar(&o.retries, "retries", 2, "Attempts per check on connection errors (HTTP error statuses are not retried)")
	flag.BoolVar(&o.followRedirects, "follow-redirects", false, "Follow redirects and report the final status instead of the gateway's 3xx")
	flag.BoolVar(&o.verifyTLS, "verify-tls", false, "Verify gateway certificates (failures are reported as 35000)")
	flag.BoolVar(&o.csvExtended, "csv-extended", false, "Append type,path,port columns to each stdout line")
	flag.BoolVar(&o.failOnError, "fail-on-error", false, "Exit 1 if any check fails (status 0, >= 500) or a namespace could not be listed")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if o.concurrency < 1 || o.retries < 1 || o.timeout <= 0 {
		fmt.Fprintln(os.Stderr, "--concurrency and --retries must be >= 1, --timeout must be > 0")
		os.Exit(2)
	}

	level := slog.LevelWarn
	if o.debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	scrapeWait, err := strconv.Atoi(getenv("SCRAPE_WAIT_SECONDS", "60"))
	if err != nil {
		fatal("invalid SCRAPE_WAIT_SECONDS", err)
	}
	runID := os.Getenv("RUN_ID")
	if runID == "" {
		runID = time.Now().UTC().Format("20060102-150405")
	}
	slog.Info("Starting", "version", version, "run_id", runID)

	cfg, err := loadKubeConfig()
	if err != nil {
		fatal("loading kube config", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fatal("creating kubernetes client", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fatal("creating dynamic client", err)
	}
	ctx := context.Background()

	m := newMetrics(prometheus.DefaultRegisterer)

	namespaces, err := getNamespaces(ctx, cs, o.namespace, o.namespacePrefix)
	if err != nil {
		fatal("selecting namespaces", err)
	}
	d := buildTasks(ctx, dyn, namespaces)
	for ns, reasons := range d.skipped {
		for reason, n := range reasons {
			m.skipped.WithLabelValues(runID, ns, reason).Add(float64(n))
		}
	}
	if len(d.tasks) == 0 {
		printSkipped(os.Stderr, d)
		slog.Error("No routes to check", "namespaces", strings.Join(namespaces, ","),
			"failed_namespaces", strings.Join(d.failedNamespaces, ","))
		os.Exit(1)
	}

	checkers := newCheckers(ctx, cs, d.tasks, o)
	res := runChecks(d.tasks, checkers, o, runID, m, os.Stdout)
	printSummary(os.Stderr, d, res, o)

	// Expose metrics on /metrics, then wait for Prometheus to scrape.
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		if err := http.ListenAndServe(fmt.Sprintf(":%d", metricsPort), nil); err != nil {
			slog.Error("metrics server failed", "err", err)
		}
	}()
	slog.Info(fmt.Sprintf("Sleeping for %d seconds to allow Prometheus to scrape before exiting...", scrapeWait))
	time.Sleep(time.Duration(scrapeWait) * time.Second)

	if o.failOnError && (res.failed > 0 || len(d.failedNamespaces) > 0) {
		os.Exit(1)
	}
}

// newCheckers returns one checker per namespace. In bypass mode the value is
// nil for namespaces without a usable gateway target; their checks report 0.
func newCheckers(ctx context.Context, cs kubernetes.Interface, tasks []task, o options) map[string]*checker {
	cfg := checkerConfig{timeout: o.timeout, retries: o.retries, followRedirects: o.followRedirects, verifyTLS: o.verifyTLS}
	checkers := map[string]*checker{}
	if !o.bypassLB {
		shared := newChecker(cfg, nil)
		for _, t := range tasks {
			checkers[t.Namespace] = shared
		}
		return checkers
	}

	ports := map[string]map[int]bool{}
	for _, t := range tasks {
		if ports[t.Namespace] == nil {
			ports[t.Namespace] = map[int]bool{}
		}
		ports[t.Namespace][t.Port] = true
	}
	for _, ns := range sortedKeys(ports) {
		target, err := findGatewayTarget(ctx, cs, ns)
		if err != nil {
			slog.Warn("Bypass: no gateway target, routes in this namespace are reported as 0", "namespace", ns, "err", err)
			checkers[ns] = nil
			continue
		}
		fmt.Fprintf(os.Stderr, "Bypass target for %s: %s\n", ns, target)
		for port := range ports[ns] {
			if _, ok := target.NodePorts[port]; !ok {
				slog.Warn("Bypass: gateway service has no nodePort for a route port, those routes are reported as 0",
					"namespace", ns, "service", target.Service, "port", port)
			}
		}
		checkers[ns] = newChecker(cfg, target)
	}
	return checkers
}

type runResult struct {
	statuses map[int]int
	failed   int
	elapsed  time.Duration
}

func runChecks(tasks []task, checkers map[string]*checker, o options, runID string, m *metrics, out io.Writer) runResult {
	bypassLabel := strconv.FormatBool(o.bypassLB)
	res := runResult{statuses: map[int]int{}}

	check := func(t task) int {
		c := checkers[t.Namespace]
		if c == nil {
			return statusError
		}
		return c.check(t)
	}
	emit := func(t task, status int) {
		m.status.WithLabelValues(t.Namespace, t.Host, t.Type, t.Path, runID, bypassLabel).Set(float64(status))
		m.counter.WithLabelValues(runID, t.Namespace, strconv.Itoa(status), bypassLabel).Inc()
		if o.csvExtended {
			fmt.Fprintf(out, "%s,%s,%s,%d,%s,%s,%d\n", runID, t.Namespace, t.Host, status, t.Type, t.Path, t.Port)
		} else {
			fmt.Fprintf(out, "%s,%s,%s,%d\n", runID, t.Namespace, t.Host, status)
		}
		res.statuses[status]++
		if failed(status) {
			res.failed++
		}
	}

	start := time.Now()
	runPool(tasks, o.concurrency, check, emit)
	res.elapsed = time.Since(start)
	return res
}

func printSummary(w io.Writer, d *discovery, res runResult, o options) {
	fmt.Fprintf(w, "Checked %d routes in %.1fs (concurrency=%d)\n", len(d.tasks), res.elapsed.Seconds(), o.concurrency)
	var parts []string
	for _, status := range sortedKeys(res.statuses) {
		parts = append(parts, fmt.Sprintf("%d=%d", status, res.statuses[status]))
	}
	fmt.Fprintf(w, "Status counts: %s (failed=%d)\n", strings.Join(parts, " "), res.failed)
	printSkipped(w, d)
	if len(d.failedNamespaces) > 0 {
		fmt.Fprintf(w, "Namespaces not checked (VirtualService list failed): %s\n", strings.Join(d.failedNamespaces, ","))
	}
}

func printSkipped(w io.Writer, d *discovery) {
	totals := map[string]int{}
	for _, reasons := range d.skipped {
		for reason, n := range reasons {
			totals[reason] += n
		}
	}
	if len(totals) == 0 {
		return
	}
	var parts []string
	for _, reason := range sortedKeys(totals) {
		parts = append(parts, fmt.Sprintf("%s=%d", reason, totals[reason]))
	}
	fmt.Fprintf(w, "Skipped: %s\n", strings.Join(parts, " "))
}

func sortedKeys[K int | string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

func loadKubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	).ClientConfig()
}
