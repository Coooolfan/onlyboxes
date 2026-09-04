package runner

import (
	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/internal/hellobuilder"
	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/config"
)

func buildHello(cfg config.Config) (*registryv1.ConnectHello, error) {
	hello := hellobuilder.Build(hellobuilder.Config{
		WorkerID: cfg.WorkerID, WorkerSecret: cfg.WorkerSecret, NodeName: cfg.NodeName,
		NodeNamePrefix: "worker-sys", ExecutorKind: "sys", Labels: cfg.Labels,
		Capabilities: []*registryv1.CapabilityDeclaration{
			{
				Name:        computerUseCapabilityDeclared,
				MaxInflight: int32(maxInflightOrDefault(cfg.ComputerUseMaxInflight)),
			},
			{
				Name:        readImageCapabilityDeclared,
				MaxInflight: int32(maxInflightOrDefault(cfg.ReadImageMaxInflight)),
			},
		},
	})
	return hello, nil
}

func maxInflightOrDefault(value int) int {
	if value <= 0 {
		return 1
	}
	return value
}
