package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Statuses other than HTTP codes. Errors use curl's exit code * 1000.
const (
	statusError         = 0     // unclassified error, or no bypass target
	statusDNSFailed     = 6000  // could not resolve host
	statusConnectFailed = 7000  // connection refused / unreachable
	statusTimeout       = 28000 // timed out
	statusSSLError      = 35000 // TLS handshake failed
	statusEmptyReply    = 52000 // connection closed without a response
	statusConnected     = 200   // tcp/tls route: connection or handshake succeeded
)

// Variable so tests can shorten it.
var retryDelay = 1 * time.Second

var errNoNodePort = errors.New("gateway service has no nodePort for this port")

type checkerConfig struct {
	timeout         time.Duration
	retries         int
	followRedirects bool
	verifyTLS       bool
}

// checker runs checks for one namespace. In bypass mode (target != nil) every
// connection goes to target.HostIP on the NodePort of the requested gateway
// port; TLS SNI and the Host header still carry the route hostname.
type checker struct {
	cfg    checkerConfig
	target *gatewayTarget
	client *http.Client
}

func newChecker(cfg checkerConfig, target *gatewayTarget) *checker {
	c := &checker{cfg: cfg, target: target}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !cfg.verifyTLS},
		// Every route is a different hostname, so keep-alive connections are
		// almost never reused and would pile up; close them after each check.
		DisableKeepAlives: true,
		DialContext:       c.dial,
	}
	c.client = &http.Client{Transport: tr, Timeout: cfg.timeout}
	if !cfg.followRedirects {
		c.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

// address maps host:port to the bypass node and NodePort. Standard mode
// connects to host:port as given.
func (c *checker) address(hostPort string) (string, error) {
	if c.target == nil {
		return hostPort, nil
	}
	_, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		return "", err
	}
	port, _ := strconv.Atoi(portStr)
	nodePort, ok := c.target.NodePorts[port]
	if !ok {
		return "", fmt.Errorf("%w: port %d on %s", errNoNodePort, port, c.target.Service)
	}
	return net.JoinHostPort(c.target.HostIP, strconv.Itoa(nodePort)), nil
}

func (c *checker) dial(ctx context.Context, network, hostPort string) (net.Conn, error) {
	addr, err := c.address(hostPort)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// check runs one task, retrying on errors (not on HTTP error statuses).
func (c *checker) check(t task) int {
	var err error
	for attempt := 1; attempt <= c.cfg.retries; attempt++ {
		var status int
		status, err = c.once(t)
		if err == nil {
			return status
		}
		slog.Debug("Check attempt failed", "attempt", attempt, "namespace", t.Namespace,
			"host", t.Host, "type", t.Type, "path", t.Path, "port", t.Port, "err", err)
		// Retrying cannot help when the port has no nodePort or the
		// certificate is rejected (--verify-tls).
		if errors.Is(err, errNoNodePort) || isCertError(err) {
			break
		}
		if attempt < c.cfg.retries {
			time.Sleep(retryDelay)
		}
	}
	return classifyError(err)
}

func (c *checker) once(t task) (int, error) {
	switch t.Type {
	case typeTCP:
		return c.checkTCP(t)
	case typeTLS:
		return c.checkTLS(t)
	default:
		return c.checkHTTP(t)
	}
}

// handshakeError marks an error that happened during the TLS handshake.
type handshakeError struct{ err error }

func (e *handshakeError) Error() string { return "tls handshake: " + e.err.Error() }
func (e *handshakeError) Unwrap() error { return e.err }

func (c *checker) checkHTTP(t task) (int, error) {
	url := "https://" + hostWithPort(t.Host, t.Port) + t.Path
	slog.Debug("Executing request: GET " + url)

	var hsErr error
	trace := &httptrace.ClientTrace{
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				hsErr = err
			}
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.client.Do(req)
	if err != nil {
		if hsErr != nil && !isTimeout(err) {
			return 0, &handshakeError{hsErr}
		}
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// checkTCP opens a TCP connection to the gateway port.
func (c *checker) checkTCP(t task) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.timeout)
	defer cancel()
	conn, err := c.dial(ctx, "tcp", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)))
	if err != nil {
		return 0, err
	}
	conn.Close()
	return statusConnected, nil
}

// checkTLS completes a TLS handshake on the gateway port with SNI = host
// (TLS passthrough routes are selected by SNI).
func (c *checker) checkTLS(t task) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.timeout)
	defer cancel()
	conn, err := c.dial(ctx, "tcp", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	tlsConn := tls.Client(conn, &tls.Config{ServerName: t.Host, InsecureSkipVerify: !c.cfg.verifyTLS})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		if isTimeout(err) {
			return 0, err
		}
		return 0, &handshakeError{err}
	}
	return statusConnected, nil
}

func hostWithPort(host string, port int) string {
	if port == defaultGatewayPort {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}

func classifyError(err error) int {
	if err == nil {
		return statusError
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return statusDNSFailed
	}
	if isTimeout(err) {
		return statusTimeout
	}
	if isTLSError(err) {
		return statusSSLError
	}
	var opErr *net.OpError
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || (errors.As(err, &opErr) && opErr.Op == "dial") {
		return statusConnectFailed
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return statusEmptyReply
	}
	return statusError
}

func isCertError(err error) bool {
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	return errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) ||
		errors.As(err, &invalidErr)
}

func isTLSError(err error) bool {
	var hs *handshakeError
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	if isCertError(err) || errors.As(err, &hs) || errors.As(err, &recordErr) || errors.As(err, &alertErr) {
		return true
	}
	// net/http reports a plain-HTTP answer to the TLS handshake as its own error.
	msg := err.Error()
	return strings.Contains(msg, "server gave HTTP response to HTTPS client") || strings.Contains(msg, "tls:")
}

// failed reports whether a status counts as a failure for --fail-on-error:
// no HTTP response at all, or a 5xx.
func failed(status int) bool {
	return status == statusError || status >= 500
}
