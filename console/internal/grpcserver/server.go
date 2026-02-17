package grpcserver

import (
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"google.golang.org/grpc"
)

func NewServer(service registryv1.WorkerRegistryServiceServer) *grpc.Server {
	server := grpc.NewServer()
	registryv1.RegisterWorkerRegistryServiceServer(server, service)
	return server
}
