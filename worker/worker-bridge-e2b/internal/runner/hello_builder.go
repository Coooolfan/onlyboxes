package runner

import (
	"errors"
	"strings"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/api/proxytoken"
	"github.com/onlyboxes/onlyboxes/worker/internal/hellobuilder"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/config"
)

func buildHello(cfg config.Config) (*registryv1.ConnectHello, error) {
	if err := validateTerminalMaxActiveSessions(cfg.TerminalMaxActiveSessions); err != nil {
		return nil, err
	}
	activeSessionCount := activeSessionCountFn()
	if activeSessionCount < 0 {
		return nil, errors.New("terminal active session count must be non-negative")
	}

	labels := make(map[string]string, len(cfg.Labels)+1)
	for key, value := range cfg.Labels {
		if strings.TrimSpace(key) == proxytoken.ProxyDirectLabel || strings.TrimSpace(key) == proxytoken.ProxyEndpointLabel {
			continue
		}
		labels[key] = value
	}
	if cfg.ProxyEnabled {
		labels[proxytoken.ProxyDirectLabel] = proxytoken.ProxyDirectE2B
	}

	capabilities := []*registryv1.CapabilityDeclaration{
		{Name: echoCapabilityName, MaxInflight: int32(cfg.EchoMaxInflight)},
		{Name: pythonExecCapabilityDeclared, MaxInflight: int32(cfg.PythonExecMaxInflight)},
		{Name: terminalExecCapabilityDeclared, MaxInflight: int32(cfg.TerminalExecMaxInflight)},
		{Name: terminalResourceCapabilityDeclared, MaxInflight: int32(cfg.TerminalResourceMaxInflight)},
	}
	if cfg.ProxyEnabled {
		capabilities = append(capabilities, &registryv1.CapabilityDeclaration{Name: terminalProxyCapabilityDeclared, MaxInflight: int32(cfg.TerminalResourceMaxInflight)})
	}

	hello := hellobuilder.Build(hellobuilder.Config{
		WorkerID: cfg.WorkerID, WorkerSecret: cfg.WorkerSecret, NodeName: cfg.NodeName,
		NodeNamePrefix: "worker-bridge-e2b", ExecutorKind: "e2b", Labels: labels,
		TerminalSessionCapacity: &registryv1.TerminalSessionCapacity{
			MaxActiveSessions:  int32(cfg.TerminalMaxActiveSessions),
			ActiveSessionCount: activeSessionCount,
		},
		Capabilities: capabilities,
	})
	return hello, nil
}
