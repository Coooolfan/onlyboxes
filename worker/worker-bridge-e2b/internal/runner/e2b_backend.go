package runner

import (
	"context"

	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b"
)

type e2bBackend interface {
	Create(context.Context, string, int) (*e2b.Sandbox, error)
	SetTimeout(context.Context, string, int) error
	Kill(context.Context, string) error
	Run(context.Context, *e2b.Sandbox, string, int) (e2b.CommandResult, error)
	ReadFile(context.Context, *e2b.Sandbox, string, int64) (e2b.File, error)
	OpenFile(context.Context, *e2b.Sandbox, string) (e2b.FileReader, error)
}

type e2bRecoveryBackend interface {
	CreateWithMetadata(context.Context, string, int, map[string]string) (*e2b.Sandbox, error)
	List(context.Context, map[string]string) ([]e2b.SandboxInfo, error)
	Connect(context.Context, string, int) (*e2b.Sandbox, error)
}
