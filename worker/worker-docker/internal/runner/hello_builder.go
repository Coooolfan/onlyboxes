package runner

import (
	"fmt"
	"strings"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
)

func buildHello(cfg config.Config) (*registryv1.ConnectHello, error) {
	nodeName := strings.TrimSpace(cfg.NodeName)
	if nodeName == "" {
		suffix := cfg.WorkerID
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		nodeName = fmt.Sprintf("worker-docker-%s", suffix)
	}

	hello := &registryv1.ConnectHello{
		NodeId:       cfg.WorkerID,
		NodeName:     nodeName,
		ExecutorKind: cfg.ExecutorKind,
		Labels:       cfg.Labels,
		Version:      cfg.Version,
		WorkerSecret: cfg.WorkerSecret,
		Capabilities: []*registryv1.CapabilityDeclaration{
			{
				Name:        echoCapabilityName,
				MaxInflight: defaultMaxInflight,
			},
			{
				Name:        pythonExecCapabilityDeclared,
				MaxInflight: defaultMaxInflight,
			},
			{
				Name:        terminalExecCapabilityDeclared,
				MaxInflight: defaultMaxInflight,
			},
			{
				Name:        terminalResourceCapabilityDeclared,
				MaxInflight: defaultMaxInflight,
			},
		},
	}
	return hello, nil
}
