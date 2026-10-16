package main

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	secretv3 "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/anypb"
)

// fakeTbot answers every requested name with a Secret of that name and
// records the names it was asked for.
type fakeTbot struct {
	secretv3.UnimplementedSecretDiscoveryServiceServer
	mu    sync.Mutex
	asked []string
}

func (f *fakeTbot) respond(req *discoveryv3.DiscoveryRequest) (*discoveryv3.DiscoveryResponse, error) {
	resp := &discoveryv3.DiscoveryResponse{VersionInfo: "1", Nonce: "n1"}
	for _, n := range req.GetResourceNames() {
		f.mu.Lock()
		f.asked = append(f.asked, n)
		f.mu.Unlock()
		a, err := anypb.New(&tlsv3.Secret{Name: n})
		if err != nil {
			return nil, err
		}
		resp.Resources = append(resp.Resources, a)
	}
	return resp, nil
}

func (f *fakeTbot) StreamSecrets(s secretv3.SecretDiscoveryService_StreamSecretsServer) error {
	for {
		req, err := s.Recv()
		if err != nil {
			return nil
		}
		resp, err := f.respond(req)
		if err != nil {
			return err
		}
		if err := s.Send(resp); err != nil {
			return err
		}
	}
}

func (f *fakeTbot) FetchSecrets(_ context.Context, req *discoveryv3.DiscoveryRequest) (*discoveryv3.DiscoveryResponse, error) {
	return f.respond(req)
}

func serve(t *testing.T, path string, impl secretv3.SecretDiscoveryServiceServer) {
	t.Helper()
	lis, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	secretv3.RegisterSecretDiscoveryServiceServer(srv, impl)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
}

func dial(t *testing.T, path string) *grpc.ClientConn {
	t.Helper()
	c, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func names(t *testing.T, resp *discoveryv3.DiscoveryResponse) []string {
	t.Helper()
	var out []string
	for _, r := range resp.GetResources() {
		var s tlsv3.Secret
		if err := r.UnmarshalTo(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s.GetName())
	}
	return out
}

func TestShim(t *testing.T) {
	dir := t.TempDir()
	tbotSock, shimSock := filepath.Join(dir, "tbot.sock"), filepath.Join(dir, "shim.sock")

	tbot := &fakeTbot{}
	serve(t, tbotSock, tbot)
	serve(t, shimSock, &shim{upstream: secretv3.NewSecretDiscoveryServiceClient(dial(t, tbotSock))})
	envoy := secretv3.NewSecretDiscoveryServiceClient(dial(t, shimSock))

	t.Run("stream", func(t *testing.T) {
		stream, err := envoy.StreamSecrets(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ ask, want string }{{"ROOTCA", "ROOTCA"}, {"default", "default"}} {
			if err := stream.Send(&discoveryv3.DiscoveryRequest{ResourceNames: []string{tc.ask}}); err != nil {
				t.Fatal(err)
			}
			resp, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if got := names(t, resp); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("asked %q: Envoy got %v, want [%s]", tc.ask, got, tc.want)
			}
			if resp.GetNonce() != "n1" || resp.GetVersionInfo() != "1" {
				t.Fatalf("nonce/version not passed through: %q/%q", resp.GetNonce(), resp.GetVersionInfo())
			}
		}
	})

	t.Run("fetch", func(t *testing.T) {
		resp, err := envoy.FetchSecrets(context.Background(), &discoveryv3.DiscoveryRequest{ResourceNames: []string{"ROOTCA"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := names(t, resp); len(got) != 1 || got[0] != "ROOTCA" {
			t.Fatalf("Envoy got %v, want [ROOTCA]", got)
		}
	})

	tbot.mu.Lock()
	defer tbot.mu.Unlock()
	for _, n := range tbot.asked {
		if n == "ROOTCA" {
			t.Fatalf("tbot was asked for ROOTCA; shim should have asked for ALL (asked: %v)", tbot.asked)
		}
	}
	t.Logf("tbot was asked for: %v", tbot.asked)
}
