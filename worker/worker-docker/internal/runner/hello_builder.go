package runner

import (
	"fmt"
	"strings"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/proxytoken"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/buildinfo"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
)

func buildHello(cfg config.Config) (*registryv1.ConnectHello, error) {
	if err := validateTerminalMaxActiveSessions(cfg.TerminalMaxActiveSessions); err != nil {
		return nil, err
	}
	activeSessionCount := activeSessionCountFn()
	if activeSessionCount < 0 {
		return nil, fmt.Errorf("terminal active session count must be non-negative")
	}

	nodeName := strings.TrimSpace(cfg.NodeName)
	if nodeName == "" {
		suffix := cfg.WorkerID
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		nodeName = fmt.Sprintf("worker-docker-%s", suffix)
	}

	labels := make(map[string]string, len(cfg.Labels)+1)
	for key, value := range cfg.Labels {
		if strings.TrimSpace(key) == proxytoken.ProxyEndpointLabel {
			continue
		}
		labels[key] = value
	}
	if cfg.ProxyEnabled {
		labels[proxytoken.ProxyEndpointLabel] = strings.TrimSpace(cfg.ProxyAdvertiseAddr)
	}

	hello := &registryv1.ConnectHello{
		NodeId:       cfg.WorkerID,
		NodeName:     nodeName,
		ExecutorKind: cfg.ExecutorKind,
		Labels:       labels,
		Version:      buildinfo.Version,
		WorkerSecret: cfg.WorkerSecret,
		TerminalSessionCapacity: &registryv1.TerminalSessionCapacity{
			MaxActiveSessions:  int32(cfg.TerminalMaxActiveSessions),
			ActiveSessionCount: activeSessionCount,
		},
		Capabilities: []*registryv1.CapabilityDeclaration{
			{
				Name:        echoCapabilityName,
				MaxInflight: int32(cfg.EchoMaxInflight),
			},
			{
				Name:        pythonExecCapabilityDeclared,
				MaxInflight: int32(cfg.PythonExecMaxInflight),
			},
			{
				Name:        terminalExecCapabilityDeclared,
				MaxInflight: int32(cfg.TerminalExecMaxInflight),
			},
			{
				Name:        terminalResourceCapabilityDeclared,
				MaxInflight: int32(cfg.TerminalResourceMaxInflight),
			},
		},
	}
	return hello, nil
}
