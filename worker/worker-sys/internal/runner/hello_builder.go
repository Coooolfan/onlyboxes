package runner

import (
	"fmt"
	"strings"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/buildinfo"
	"github.com/onlyboxes/onlyboxes/worker/worker-sys/internal/config"
)

func buildHello(cfg config.Config) (*registryv1.ConnectHello, error) {
	nodeName := strings.TrimSpace(cfg.NodeName)
	if nodeName == "" {
		suffix := cfg.WorkerID
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		nodeName = fmt.Sprintf("worker-sys-%s", suffix)
	}

	hello := &registryv1.ConnectHello{
		NodeId:       cfg.WorkerID,
		NodeName:     nodeName,
		ExecutorKind: cfg.ExecutorKind,
		Labels:       cfg.Labels,
		Version:      buildinfo.Version,
		WorkerSecret: cfg.WorkerSecret,
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
	}
	return hello, nil
}

func maxInflightOrDefault(value int) int {
	if value <= 0 {
		return 1
	}
	return value
}
