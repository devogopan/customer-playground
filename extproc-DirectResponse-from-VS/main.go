package main

import (
	"io"
	"log"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

type extProcServer struct {
	extProcPb.UnimplementedExternalProcessorServer
}

// Process handles external processing requests from Envoy.
// It listens for incoming requests, modifies response headers,
// and sends the updated response back to Envoy.
//
// When a request with response headers is received, it adds a custom header
// "x-extproc-hello" with the value "Hello from ext_proc" and returns the modified headers.
//
// Note: The `RawValue` field is used instead of `Value` because it supports
// setting the header value as a byte slice, allowing precise handling of binary data.
//
// This function is called once per HTTP request to process gRPC messages from Envoy.
// It exits when an error occurs while receiving or sending messages.

func (s *extProcServer) Process(
	srv extProcPb.ExternalProcessor_ProcessServer,
) error {
	for {
		// 1. Receive a message from Envoy
		req, err := srv.Recv()
		if err != nil {
			// Use io.EOF to detect when the stream is closed
			if err == io.EOF {
				return nil
			}
			return status.Errorf(codes.Unknown, "error receiving request: %v", err)
		}

		log.Printf("Received request: %+v\n", req.GetRequest())

		// 2. Decide what to do based on the message type
		var resp *extProcPb.ProcessingResponse
		switch req.GetRequest().(type) {

		case *extProcPb.ProcessingRequest_RequestHeaders:
			log.Println(">>> Received request headers")
			// Immediately send back a response to let the request continue
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extProcPb.HeadersResponse{
						Response: &extProcPb.CommonResponse{},
					},
				},
			}

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			log.Println(">>> Received response headers: Modifying...")
			// This is the correct place to modify the response headers
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extProcPb.HeadersResponse{
						Response: &extProcPb.CommonResponse{
							HeaderMutation: &extProcPb.HeaderMutation{
								SetHeaders: []*configPb.HeaderValueOption{
									{
										Header: &configPb.HeaderValue{
											Key:      "x-extproc-hello",
											RawValue: []byte("Hello from ext_proc"),
										},
									},
								},
							},
						},
					},
				},
			}

		default:
			// For other message types (body, trailers), just continue processing
			// without modification by sending an empty CommonResponse.
			// This is crucial to prevent the connection from stalling.
			log.Println(">>> Received other phase: Continuing.")
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ImmediateResponse{
					ImmediateResponse: &extProcPb.ImmediateResponse{},
				},
			}
		}

		// 3. Send the response back to Envoy
		if err := srv.Send(resp); err != nil {
			log.Printf("Error sending response: %v", err)
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	// Register the ExternalProcessorServer with the gRPC server.
	extProcPb.RegisterExternalProcessorServer(grpcServer, &extProcServer{})

	log.Println("Starting gRPC server on :9000...")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
