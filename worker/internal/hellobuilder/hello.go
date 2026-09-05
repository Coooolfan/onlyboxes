package hellobuilder

import (
	"fmt"
	"strings"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/internal/buildinfo"
)

type Config struct {
	WorkerID                string
	WorkerSecret            string
	NodeName                string
	NodeNamePrefix          string
	ExecutorKind            string
	Labels                  map[string]string
	Capabilities            []*registryv1.CapabilityDeclaration
	TerminalSessionCapacity *registryv1.TerminalSessionCapacity
}

func Build(config Config) *registryv1.ConnectHello {
	nodeName := strings.TrimSpace(config.NodeName)
	if nodeName == "" {
		suffix := config.WorkerID
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		nodeName = fmt.Sprintf("%s-%s", config.NodeNamePrefix, suffix)
	}
	return &registryv1.ConnectHello{
		NodeId: config.WorkerID, NodeName: nodeName, ExecutorKind: config.ExecutorKind,
		Labels: config.Labels, Version: buildinfo.Version, WorkerSecret: config.WorkerSecret,
		TerminalSessionCapacity: config.TerminalSessionCapacity, Capabilities: config.Capabilities,
	}
}
