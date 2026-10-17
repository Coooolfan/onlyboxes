package grpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"google.golang.org/protobuf/proto"
)

const maxPersistencePayload = 64 << 10

// PersistenceControlError distinguishes a local admission failure from an
// uncertain backend outcome. Once enqueued, a timeout/disconnect requires
// inspect/reconciliation; it never authorizes reassigning execution ownership.
type PersistenceControlError struct {
	MayHaveExecuted bool
	Err             error
}

func (e *PersistenceControlError) Error() string { return e.Err.Error() }
func (e *PersistenceControlError) Unwrap() error { return e.Err }

type persistenceOutcome struct {
	report *registryv1.SandboxPersistenceReport
	err    error
}

type pendingPersistenceControl struct {
	control *registryv1.SandboxPersistenceControl
	result  chan persistenceOutcome
}

// The declaration and pending deliveries belong to a single authenticated
// connection. Reconnecting never inherits a previous connection's replies.
type persistenceSession struct {
	mu               sync.Mutex
	capabilities     *registryv1.SandboxPersistenceCapabilities
	pending          map[string]pendingPersistenceControl
	closed           bool
	reportedInflight int32
	availableBytes   int64
	reservedBytes    int64
	reservations     int32
}

func (p *persistenceSession) initialize(capabilities *registryv1.SandboxPersistenceCapabilities) {
	p.pending = make(map[string]pendingPersistenceControl)
	if capabilities != nil {
		p.capabilities = proto.Clone(capabilities).(*registryv1.SandboxPersistenceCapabilities)
		p.availableBytes = capabilities.GetAvailableStagingBytes()
	}
}

func validatePersistenceCapabilities(hello *registryv1.ConnectHello) error {
	c := hello.GetSandboxPersistence()
	if c == nil || !c.GetEnabled() {
		return nil
	}
	if c.GetRuntime() != "docker" && c.GetRuntime() != "boxlite" {
		return errors.New("sandbox_persistence requires a supported runtime")
	}
	if strings.TrimSpace(hello.GetExecutorKind()) != c.GetRuntime() || strings.TrimSpace(c.GetRuntimeVersion()) == "" || len(c.GetRuntimeVersion()) > 128 {
		return errors.New("sandbox_persistence runtime identity is invalid")
	}
	if c.GetGuestPlatform() != "linux/amd64" && c.GetGuestPlatform() != "linux/arm64" {
		return errors.New("sandbox_persistence guest platform is unsupported")
	}
	if c.GetMaxInflight() <= 0 || c.GetMaxInflight() > 64 || c.GetAvailableStagingBytes() < 0 {
		return errors.New("sandbox_persistence capacity is invalid")
	}
	if hello.GetTerminalSessionCapacity() == nil {
		return errors.New("sandbox_persistence requires terminal session capacity")
	}
	format := "docker-rootfs-v1"
	if c.GetRuntime() == "boxlite" {
		format = "boxlite-archive-v1"
	}
	if len(c.GetReadFormats())+len(c.GetWriteFormats()) == 0 {
		return errors.New("sandbox_persistence must declare archive formats")
	}
	for _, formats := range [][]string{c.GetReadFormats(), c.GetWriteFormats()} {
		if len(formats) > 8 {
			return errors.New("too many sandbox persistence formats")
		}
		seen := make(map[string]bool)
		for _, f := range formats {
			if f != format || seen[f] {
				return errors.New("unsupported or duplicate sandbox persistence format")
			}
			seen[f] = true
		}
	}
	return nil
}

func (p *persistenceSession) validateUsage(usage *registryv1.SandboxPersistenceUsage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if usage == nil {
		if p.capabilities.GetEnabled() {
			return errors.New("enabled sandbox persistence requires heartbeat usage")
		}
		return nil
	}
	if !p.capabilities.GetEnabled() || usage.GetInflight() < 0 || usage.GetAvailableStagingBytes() < 0 {
		return errors.New("sandbox persistence usage is invalid or undeclared")
	}
	return nil
}

func (p *persistenceSession) updateUsage(usage *registryv1.SandboxPersistenceUsage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if usage != nil {
		p.reportedInflight = usage.GetInflight()
		p.availableBytes = usage.GetAvailableStagingBytes()
	}
}

func validatePersistenceControl(c *registryv1.SandboxPersistenceControl, now time.Time) error {
	if c == nil || strings.TrimSpace(c.GetRequestId()) == "" || len(c.GetRequestId()) > 128 || strings.TrimSpace(c.GetOperationId()) == "" || len(c.GetOperationId()) > 128 || len(c.GetScopedSessionId()) > 1024 || !isValidPersistedTerminalSessionID(c.GetScopedSessionId()) || c.GetGeneration() <= 0 || c.GetDeadlineUnixMs() <= now.UnixMilli() {
		return errors.New("invalid sandbox persistence control identity or deadline")
	}
	if c.GetAction() < registryv1.SandboxPersistenceControl_PREPARE_SNAPSHOT || c.GetAction() > registryv1.SandboxPersistenceControl_INSPECT {
		return errors.New("unsupported sandbox persistence action")
	}
	if len(c.GetPayloadJson()) > maxPersistencePayload || !json.Valid(c.GetPayloadJson()) {
		return errors.New("sandbox persistence payload must be bounded valid JSON")
	}
	return nil
}

func (p *persistenceSession) register(control *registryv1.SandboxPersistenceControl) (<-chan persistenceOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.capabilities.GetEnabled() {
		return nil, ErrNoCapabilityWorker
	}
	if _, exists := p.pending[control.GetRequestId()]; exists {
		return nil, errors.New("duplicate persistence delivery request_id")
	}
	// Inspect/cancel remain available when a worker reports all operation slots
	// occupied. Pending control messages have an independent bounded limit.
	if len(p.pending) >= int(p.capabilities.GetMaxInflight()) {
		return nil, ErrNoWorkerCapacity
	}
	pending := pendingPersistenceControl{control: control, result: make(chan persistenceOutcome, 1)}
	p.pending[control.GetRequestId()] = pending
	return pending.result, nil
}

func (p *persistenceSession) unregister(requestID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, requestID)
}

func (p *persistenceSession) resolve(report *registryv1.SandboxPersistenceReport) error {
	if report == nil || strings.TrimSpace(report.GetRequestId()) == "" || len(report.GetRequestId()) > 128 || len(report.GetPhase()) > 128 || len(report.GetPayloadJson()) > maxPersistencePayload || len(report.GetError().GetCode()) > 128 || len(report.GetError().GetMessage()) > 4096 {
		return errors.New("invalid sandbox persistence report")
	}
	if len(report.GetPayloadJson()) > 0 && !json.Valid(report.GetPayloadJson()) {
		return errors.New("sandbox persistence report contains invalid JSON")
	}
	if !report.GetCompleted() && report.GetError() != nil {
		return errors.New("sandbox persistence errors must be final reports")
	}
	if report.GetCompleted() && ((report.GetError() == nil && len(report.GetPayloadJson()) == 0) || (report.GetError() != nil && (report.GetError().GetCode() == "" || len(report.GetPayloadJson()) != 0))) {
		return errors.New("sandbox persistence final report requires exactly one result or error")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.capabilities.GetEnabled() {
		return errors.New("sandbox persistence report is undeclared")
	}
	pending, ok := p.pending[report.GetRequestId()]
	if !ok {
		// Late/duplicate results cannot mutate a durable operation. The lifecycle
		// coordinator must inspect again with a fresh request after a timeout.
		return nil
	}
	c := pending.control
	if c.GetOperationId() != report.GetOperationId() || c.GetScopedSessionId() != report.GetScopedSessionId() || c.GetGeneration() != report.GetGeneration() || c.GetAction() != report.GetAction() {
		return errors.New("sandbox persistence report does not match its control")
	}
	if !report.GetCompleted() {
		return nil
	}
	delete(p.pending, report.GetRequestId())
	pending.result <- persistenceOutcome{report: proto.Clone(report).(*registryv1.SandboxPersistenceReport)}
	return nil
}

func (p *persistenceSession) close(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for id, pending := range p.pending {
		pending.result <- persistenceOutcome{err: err}
		delete(p.pending, id)
	}
}

// DispatchPersistenceControl sends a bounded internal lifecycle message. The
// caller must first own the durable operation and generation. This method does
// not change session metadata, publish routes or retry side effects.
func (s *RegistryService) DispatchPersistenceControl(ctx context.Context, workerID string, control *registryv1.SandboxPersistenceControl) (*registryv1.SandboxPersistenceReport, error) {
	return s.dispatchPersistenceControl(ctx, s.getSession(workerID), control)
}

func (s *RegistryService) dispatchPersistenceControl(ctx context.Context, session *activeSession, control *registryv1.SandboxPersistenceControl) (*registryv1.SandboxPersistenceReport, error) {
	localError := func(err error) (*registryv1.SandboxPersistenceReport, error) {
		return nil, &PersistenceControlError{Err: err}
	}
	if err := ctx.Err(); err != nil {
		return localError(err)
	}
	if err := validatePersistenceControl(control, s.nowFn()); err != nil {
		return localError(err)
	}
	if !s.persistenceSessionOnline(session) {
		return localError(ErrNoCapabilityWorker)
	}
	control = proto.Clone(control).(*registryv1.SandboxPersistenceControl)
	result, err := session.persistence.register(control)
	if err != nil {
		return localError(err)
	}
	defer session.persistence.unregister(control.GetRequestId())
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(control.GetDeadlineUnixMs()))
	defer cancel()
	if err := ctx.Err(); err != nil {
		return localError(err)
	}
	if err := session.enqueueControl(ctx, &registryv1.ConnectResponse{Payload: &registryv1.ConnectResponse_SandboxPersistenceControl{SandboxPersistenceControl: control}}); err != nil {
		return localError(err)
	}
	uncertainError := func(err error) (*registryv1.SandboxPersistenceReport, error) {
		return nil, &PersistenceControlError{MayHaveExecuted: true, Err: err}
	}
	select {
	case outcome := <-result:
		if outcome.err != nil {
			return uncertainError(outcome.err)
		}
		if e := outcome.report.GetError(); e != nil {
			return outcome.report, &PersistenceControlError{MayHaveExecuted: true, Err: &CommandExecutionError{Code: e.GetCode(), Message: e.GetMessage()}}
		}
		return outcome.report, nil
	case <-ctx.Done():
		return uncertainError(ctx.Err())
	case <-session.done:
		return uncertainError(session.sessionError())
	}
}

func (s *RegistryService) persistenceSessionOnline(session *activeSession) bool {
	if session == nil || !session.isReady() {
		return false
	}
	select {
	case <-session.done:
		return false
	default:
	}
	return s.nowFn().Sub(time.UnixMilli(session.lastHeartbeatUnixMs.Load())) <= time.Duration(s.offlineTTLSec)*time.Second
}
