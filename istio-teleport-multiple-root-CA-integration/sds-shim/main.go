// Command sds-shim sits between istio-proxy (Envoy) and tbot on the Envoy SDS
// API. Istio always asks for the trust bundle as "ROOTCA", which tbot fills
// with its own trust domain only. tbot serves own + federated trust domains as
// "ALL". The shim requests "ALL" upstream and returns it to Envoy as "ROOTCA".
// Every other secret (e.g. "default") passes through unchanged.
//
// It must run inside the workload pod: tbot attests the process that connects
// to its socket, and a container in the same pod resolves to the same pod.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	secretv3 "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	istioRootName = "ROOTCA" // name Istio's Envoy requests
	tbotAllName   = "ALL"    // name tbot uses for own + federated bundles
)

type shim struct {
	secretv3.UnimplementedSecretDiscoveryServiceServer
	upstream secretv3.SecretDiscoveryServiceClient
}

// toUpstream rewrites requested names before forwarding to tbot.
func toUpstream(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		if n == istioRootName {
			n = tbotAllName
		}
		out[i] = n
	}
	return out
}

// toDownstream renames the "ALL" secret back to "ROOTCA" so Envoy accepts it.
func toDownstream(resp *discoveryv3.DiscoveryResponse) error {
	for _, r := range resp.GetResources() {
		var s tlsv3.Secret
		if err := r.UnmarshalTo(&s); err != nil {
			return err
		}
		if s.GetName() != tbotAllName {
			continue
		}
		s.Name = istioRootName
		if err := r.MarshalFrom(&s); err != nil {
			return err
		}
	}
	return nil
}

// StreamSecrets proxies Envoy's state-of-the-world SDS stream (the API type
// Istio configures for the sds-grpc cluster).
func (s *shim) StreamSecrets(down secretv3.SecretDiscoveryService_StreamSecretsServer) error {
	ctx, cancel := context.WithCancel(down.Context())
	defer cancel()

	up, err := s.upstream.StreamSecrets(ctx)
	if err != nil {
		return err
	}

	errc := make(chan error, 2)
	go func() {
		for {
			req, err := down.Recv()
			if err != nil {
				errc <- err
				return
			}
			req.ResourceNames = toUpstream(req.GetResourceNames())
			if err := up.Send(req); err != nil {
				errc <- err
				return
			}
		}
	}()
	go func() {
		for {
			resp, err := up.Recv()
			if err == nil {
				err = toDownstream(resp)
			}
			if err == nil {
				err = down.Send(resp)
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()

	if err := <-errc; !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *shim) FetchSecrets(ctx context.Context, req *discoveryv3.DiscoveryRequest) (*discoveryv3.DiscoveryResponse, error) {
	req.ResourceNames = toUpstream(req.GetResourceNames())
	resp, err := s.upstream.FetchSecrets(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, toDownstream(resp)
}

func main() {
	listen := flag.String("listen", "/var/run/secrets/workload-spiffe-uds/socket", "socket Envoy connects to")
	upstream := flag.String("upstream", "/run/tbot-upstream/socket", "tbot workload-identity-api socket")
	check := flag.String("check", "", "exit 0 if this socket accepts connections (for a startupProbe), then quit")
	flag.Parse()

	if *check != "" {
		c, err := net.DialTimeout("unix", *check, time.Second)
		if err != nil {
			os.Exit(1)
		}
		c.Close()
		return
	}

	conn, err := grpc.NewClient("unix://"+*upstream, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("upstream: %v", err)
	}
	defer conn.Close()

	// If something already serves this path, istio-agent most likely started
	// first and became the SDS server. Fail loudly instead of replacing it.
	if c, err := net.DialTimeout("unix", *listen, time.Second); err == nil {
		c.Close()
		log.Fatalf("%s is already being served by another process; istio-proxy probably started before sds-shim", *listen)
	}
	if err := os.Remove(*listen); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("remove stale socket: %v", err)
	}
	lis, err := net.Listen("unix", *listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	// istio-proxy runs as a different UID. Access is limited by which
	// containers mount the volume, not by file mode.
	if err := os.Chmod(*listen, 0o666); err != nil {
		log.Fatalf("chmod: %v", err)
	}

	srv := grpc.NewServer()
	secretv3.RegisterSecretDiscoveryServiceServer(srv, &shim{upstream: secretv3.NewSecretDiscoveryServiceClient(conn)})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		srv.GracefulStop()
	}()

	log.Printf("sds-shim: %s -> %s (%s served as %s)", *listen, *upstream, tbotAllName, istioRootName)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
