package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"

	"reverse-proxy/testgrpc/echopb"
)

type server struct {
	echopb.UnimplementedEchoServiceServer
}

func (s *server) Echo(ctx context.Context, req *echopb.EchoRequest) (*echopb.EchoResponse, error) {
	log.Printf("received: %q", req.GetMessage())
	return &echopb.EchoResponse{
		Message: "echo: " + req.GetMessage(),
	}, nil
}

func main() {
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatal(err)
	}

	cert, err := tls.LoadX509KeyPair("certs/cert.pem", "certs/key.pem")
	if err != nil {
		log.Fatal(err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	creds := credentials.NewTLS(tlsConfig)

	grpcServer := grpc.NewServer(grpc.Creds(creds))
	echopb.RegisterEchoServiceServer(grpcServer, &server{})
	reflection.Register(grpcServer)

	log.Println("gRPC TLS backend listening on :50051")
	log.Fatal(grpcServer.Serve(lis))
}
