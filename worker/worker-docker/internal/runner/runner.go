package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os/exec"
	"strconv"
	"strings"
	"time"

	registryv1 "github.com/onlyboxes/onlyboxes/api/gen/go/registry/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	minHeartbeatInterval           = 1 * time.Second
	initialReconnectDelay          = 1 * time.Second
	maxReconnectDelay              = 15 * time.Second
	echoCapabilityName             = "echo"
	pythonExecCapabilityName       = "pythonexec"
	pythonExecCapabilityDeclared   = "pythonExec"
	defaultPythonExecDockerImage   = "python:slim"
	defaultPythonExecMemoryLimit   = "256m"
	defaultPythonExecCPULimit      = "1.0"
	defaultPythonExecPidsLimit     = 128
	defaultTerminalExecDockerImage = "python:slim"
	defaultTerminalExecMemoryLimit = "256m"
	defaultTerminalExecCPULimit    = "1.0"
	defaultTerminalExecPidsLimit   = 128
	pythonExecContainerPrefix      = "onlyboxes-pythonexec-"
	pythonExecManagedLabel         = "onlyboxes.managed=true"
	pythonExecCapabilityLabel      = "onlyboxes.capability=pythonExec"
	pythonExecRuntimeLabel         = "onlyboxes.runtime=worker-docker"
	pythonExecCleanupTimeout       = 3 * time.Second
	pythonExecInspectTimeout       = 2 * time.Second
	defaultMaxInflight             = 4
)

var waitReconnect = waitReconnectDelay
var applyJitter = jitterDuration
var runPythonExec = newPythonExecRunner("").Execute
var runTerminalExec = runTerminalExecUnavailable
var runTerminalResource = runTerminalResourceUnavailable
var runDockerCommand = runDockerCommandCLI
var pythonExecContainerNameFn = newPythonExecContainerName

type pythonExecRunner struct {
	dockerImage string
}

func newPythonExecRunner(dockerImage string) *pythonExecRunner {
	return &pythonExecRunner{
		dockerImage: dockerImage,
	}
}

func (r *pythonExecRunner) Execute(ctx context.Context, code string) (pythonExecRunResult, error) {
	if r == nil {
		return runPythonExecInDockerWithImage(ctx, "", code)
	}
	return runPythonExecInDockerWithImage(ctx, r.dockerImage, code)
}

func Run(ctx context.Context, cfg config.Config) error {
	if strings.TrimSpace(cfg.WorkerID) == "" {
		return errors.New("WORKER_ID is required")
	}
	if strings.TrimSpace(cfg.WorkerSecret) == "" {
		return errors.New("WORKER_SECRET is required")
	}

	terminalManager := newTerminalSessionManager(terminalSessionManagerConfig{
		LeaseMinSec:      cfg.TerminalLeaseMinSec,
		LeaseMaxSec:      cfg.TerminalLeaseMaxSec,
		LeaseDefaultSec:  cfg.TerminalLeaseDefaultSec,
		OutputLimitBytes: cfg.TerminalOutputLimitBytes,
		DockerImage:      cfg.TerminalExecDockerImage,
		MemoryLimit:      defaultTerminalExecMemoryLimit,
		CPULimit:         defaultTerminalExecCPULimit,
		PidsLimit:        defaultTerminalExecPidsLimit,
	})
	pythonRunner := newPythonExecRunner(cfg.PythonExecDockerImage)
	originalRunPythonExec := runPythonExec
	runPythonExec = pythonRunner.Execute
	originalRunTerminalExec := runTerminalExec
	runTerminalExec = terminalManager.Execute
	originalRunTerminalResource := runTerminalResource
	runTerminalResource = terminalManager.ResolveResource
	defer func() {
		runPythonExec = originalRunPythonExec
		runTerminalExec = originalRunTerminalExec
		runTerminalResource = originalRunTerminalResource
		terminalManager.Close()
	}()

	reconnectDelay := initialReconnectDelay
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := runSession(ctx, cfg)
		if err == nil {
			return nil
		}

		if errCtx := ctx.Err(); errCtx != nil {
			return errCtx
		}

		if status.Code(err) == codes.FailedPrecondition {
			log.Printf("registry session replaced for node_id=%s, reconnecting", cfg.WorkerID)
			reconnectDelay = initialReconnectDelay
		} else {
			log.Printf("registry session interrupted: %v", err)
		}

		if err := waitReconnect(ctx, reconnectDelay); err != nil {
			return err
		}
		reconnectDelay = nextReconnectDelay(reconnectDelay)
	}
}

func runSession(ctx context.Context, cfg config.Config) error {
	conn, err := dial(ctx, cfg)
	if err != nil {
		return fmt.Errorf("dial console: %w", err)
	}
	defer conn.Close()

	client := registryv1.NewWorkerRegistryServiceClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("open connect stream: %w", err)
	}
	defer stream.CloseSend()

	hello, err := buildHello(cfg)
	if err != nil {
		return fmt.Errorf("build hello: %w", err)
	}

	if err := stream.Send(&registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_Hello{Hello: hello},
	}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	resp, err := recvWithTimeout(ctx, cfg.CallTimeout, stream.Recv)
	if err != nil {
		return fmt.Errorf("recv connect_ack: %w", err)
	}
	ack := resp.GetConnectAck()
	if ack == nil {
		return fmt.Errorf("unexpected first response frame")
	}
	sessionID := strings.TrimSpace(ack.GetSessionId())
	if sessionID == "" {
		return fmt.Errorf("connect_ack.session_id is required")
	}

	heartbeatInterval := durationFromServer(ack.GetHeartbeatIntervalSec(), cfg.HeartbeatInterval)
	log.Printf("worker connected: node_id=%s node_name=%s session_id=%s", hello.GetNodeId(), hello.GetNodeName(), sessionID)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outbound := make(chan *registryv1.ConnectRequest, 64)
	heartbeatAckCh := make(chan *registryv1.HeartbeatAck, 16)
	sessionErrCh := make(chan error, 4)

	go senderLoop(sessionCtx, stream, outbound, sessionErrCh)
	go receiverLoop(sessionCtx, stream, outbound, heartbeatAckCh, sessionErrCh, cfg.WorkerID)

	return heartbeatLoop(sessionCtx, outbound, heartbeatAckCh, sessionErrCh, cfg, sessionID, heartbeatInterval)
}

func dial(ctx context.Context, cfg config.Config) (*grpc.ClientConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return grpc.NewClient(
		cfg.ConsoleGRPCTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

func senderLoop(
	ctx context.Context,
	stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse],
	outbound <-chan *registryv1.ConnectRequest,
	errCh chan<- error,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-outbound:
			if req == nil {
				continue
			}
			if err := stream.Send(req); err != nil {
				reportSessionErr(errCh, fmt.Errorf("stream send failed: %w", err))
				return
			}
		}
	}
}

func receiverLoop(
	ctx context.Context,
	stream grpc.BidiStreamingClient[registryv1.ConnectRequest, registryv1.ConnectResponse],
	outbound chan<- *registryv1.ConnectRequest,
	heartbeatAckCh chan<- *registryv1.HeartbeatAck,
	errCh chan<- error,
	nodeID string,
) {
	for {
		resp, err := stream.Recv()
		if err != nil {
			reportSessionErr(errCh, fmt.Errorf("stream receive failed: %w", err))
			return
		}

		switch {
		case resp.GetHeartbeatAck() != nil:
			select {
			case <-ctx.Done():
				return
			case heartbeatAckCh <- resp.GetHeartbeatAck():
			}
		case resp.GetCommandDispatch() != nil:
			dispatch := resp.GetCommandDispatch()
			capability := strings.TrimSpace(strings.ToLower(dispatch.GetCapability()))
			commandID := strings.TrimSpace(dispatch.GetCommandId())
			switch {
			case capability == echoCapabilityName:
				log.Printf(
					"command dispatch received: node_id=%s command_id=%s capability=%s payload_len=%d",
					nodeID,
					commandID,
					capability,
					len(dispatch.GetPayloadJson()),
				)
			case capability == pythonExecCapabilityName:
				log.Printf(
					"command dispatch received: node_id=%s command_id=%s capability=%s payload_len=%d",
					nodeID,
					commandID,
					capability,
					len(dispatch.GetPayloadJson()),
				)
			case capability == terminalExecCapabilityName:
				log.Printf(
					"command dispatch received: node_id=%s command_id=%s capability=%s payload_len=%d",
					nodeID,
					commandID,
					capability,
					len(dispatch.GetPayloadJson()),
				)
			case capability == terminalResourceCapabilityName:
				log.Printf(
					"command dispatch received: node_id=%s command_id=%s capability=%s payload_len=%d",
					nodeID,
					commandID,
					capability,
					len(dispatch.GetPayloadJson()),
				)
			default:
				log.Printf(
					"command dispatch received: node_id=%s command_id=%s capability=%s",
					nodeID,
					commandID,
					capability,
				)
			}

			dispatchCopy, ok := proto.Clone(dispatch).(*registryv1.CommandDispatch)
			if !ok || dispatchCopy == nil {
				reportSessionErr(errCh, errors.New("clone command dispatch failed"))
				return
			}

			go func(dispatch *registryv1.CommandDispatch) {
				resultReq := buildCommandResultWithContext(ctx, dispatch)
				if sendErr := enqueueRequest(ctx, outbound, resultReq); sendErr != nil {
					if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
						return
					}
					reportSessionErr(errCh, fmt.Errorf("enqueue command result: %w", sendErr))
				}
			}(dispatchCopy)
		default:
			reportSessionErr(errCh, errors.New("unexpected response frame"))
			return
		}
	}
}

func heartbeatLoop(
	ctx context.Context,
	outbound chan<- *registryv1.ConnectRequest,
	heartbeatAckCh <-chan *registryv1.HeartbeatAck,
	sessionErrCh <-chan error,
	cfg config.Config,
	sessionID string,
	heartbeatInterval time.Duration,
) error {
	interval := heartbeatInterval

	for {
		waitFor := applyJitter(interval, cfg.HeartbeatJitter)
		timer := time.NewTimer(waitFor)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-sessionErrCh:
			timer.Stop()
			return err
		case <-timer.C:
		}

		if err := enqueueRequest(ctx, outbound, &registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_Heartbeat{
				Heartbeat: &registryv1.HeartbeatFrame{
					NodeId:    cfg.WorkerID,
					SessionId: sessionID,
				},
			},
		}); err != nil {
			return fmt.Errorf("enqueue heartbeat: %w", err)
		}

		ackTimer := time.NewTimer(cfg.CallTimeout)
		waitAck := true
		for waitAck {
			select {
			case <-ctx.Done():
				ackTimer.Stop()
				return ctx.Err()
			case err := <-sessionErrCh:
				ackTimer.Stop()
				return err
			case <-ackTimer.C:
				return context.DeadlineExceeded
			case heartbeatAck := <-heartbeatAckCh:
				ackTimer.Stop()
				interval = durationFromServer(heartbeatAck.GetHeartbeatIntervalSec(), interval)
				waitAck = false
			}
		}
	}
}

func buildCommandResult(dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	return buildCommandResultWithContext(context.Background(), dispatch)
}

func buildCommandResultWithContext(baseCtx context.Context, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	commandID := strings.TrimSpace(dispatch.GetCommandId())
	if commandID == "" {
		return &registryv1.ConnectRequest{
			Payload: &registryv1.ConnectRequest_CommandResult{
				CommandResult: &registryv1.CommandResult{
					CommandId: commandID,
					Error: &registryv1.CommandError{
						Code:    "invalid_command_id",
						Message: "command_id is required",
					},
				},
			},
		}
	}

	if deadline := dispatch.GetDeadlineUnixMs(); deadline > 0 && time.Now().UnixMilli() > deadline {
		return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
	}

	capability := strings.TrimSpace(strings.ToLower(dispatch.GetCapability()))
	switch capability {
	case echoCapabilityName:
		return buildEchoCommandResult(commandID, dispatch)
	case pythonExecCapabilityName:
		return buildPythonExecCommandResult(baseCtx, commandID, dispatch)
	case terminalExecCapabilityName:
		return buildTerminalExecCommandResult(baseCtx, commandID, dispatch)
	case terminalResourceCapabilityName:
		return buildTerminalResourceCommandResult(baseCtx, commandID, dispatch)
	default:
		return commandErrorResult(commandID, "unsupported_capability", fmt.Sprintf("capability %q is not supported", dispatch.GetCapability()))
	}
}

func buildEchoCommandResult(commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	message := ""
	resultPayload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(resultPayload) > 0 {
		decoded := struct {
			Message string `json:"message"`
		}{}
		if err := json.Unmarshal(resultPayload, &decoded); err != nil {
			return commandErrorResult(commandID, "invalid_payload", "payload_json is not valid echo payload")
		}
		message = decoded.Message
	}
	if strings.TrimSpace(message) == "" {
		return commandErrorResult(commandID, "invalid_payload", "echo payload is required")
	}
	if len(resultPayload) == 0 {
		encoded, err := json.Marshal(struct {
			Message string `json:"message"`
		}{
			Message: message,
		})
		if err != nil {
			return commandErrorResult(commandID, "encode_failed", "failed to encode echo payload")
		}
		resultPayload = encoded
	}

	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				PayloadJson:     resultPayload,
				CompletedUnixMs: time.Now().UnixMilli(),
			},
		},
	}
}

type pythonExecPayload struct {
	Code string `json:"code"`
}

type pythonExecResult struct {
	Output   string `json:"output"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type pythonExecRunResult struct {
	Output   string
	Stderr   string
	ExitCode int
}

func buildPythonExecCommandResult(baseCtx context.Context, commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	payload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(payload) == 0 {
		return commandErrorResult(commandID, "invalid_payload", "pythonExec payload is required")
	}

	decoded := pythonExecPayload{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return commandErrorResult(commandID, "invalid_payload", "payload_json is not valid pythonExec payload")
	}
	if strings.TrimSpace(decoded.Code) == "" {
		return commandErrorResult(commandID, "invalid_payload", "pythonExec code is required")
	}

	commandCtx := baseCtx
	if commandCtx == nil {
		commandCtx = context.Background()
	}
	cancel := func() {}
	if deadlineUnixMS := dispatch.GetDeadlineUnixMs(); deadlineUnixMS > 0 {
		commandCtx, cancel = context.WithDeadline(commandCtx, time.UnixMilli(deadlineUnixMS))
	}
	defer cancel()

	execResult, err := runPythonExec(commandCtx, decoded.Code)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
		}
		return commandErrorResult(commandID, "execution_failed", fmt.Sprintf("pythonExec execution failed: %v", err))
	}

	resultPayload, err := json.Marshal(pythonExecResult{
		Output:   execResult.Output,
		Stderr:   execResult.Stderr,
		ExitCode: execResult.ExitCode,
	})
	if err != nil {
		return commandErrorResult(commandID, "encode_failed", "failed to encode pythonExec payload")
	}

	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				PayloadJson:     resultPayload,
				CompletedUnixMs: time.Now().UnixMilli(),
			},
		},
	}
}

func buildTerminalExecCommandResult(baseCtx context.Context, commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	payload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(payload) == 0 {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "terminalExec payload is required")
	}

	decoded := terminalExecPayload{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "payload_json is not valid terminalExec payload")
	}
	if strings.TrimSpace(decoded.Command) == "" {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "terminalExec command is required")
	}

	commandCtx := baseCtx
	if commandCtx == nil {
		commandCtx = context.Background()
	}
	cancel := func() {}
	if deadlineUnixMS := dispatch.GetDeadlineUnixMs(); deadlineUnixMS > 0 {
		commandCtx, cancel = context.WithDeadline(commandCtx, time.UnixMilli(deadlineUnixMS))
	}
	defer cancel()

	execResult, err := runTerminalExec(commandCtx, terminalExecRequest{
		Command:         decoded.Command,
		SessionID:       decoded.SessionID,
		CreateIfMissing: decoded.CreateIfMissing,
		LeaseTTLSec:     decoded.LeaseTTLSec,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
		}
		var terminalErr *terminalExecError
		if errors.As(err, &terminalErr) {
			return commandErrorResult(commandID, terminalErr.Code(), terminalErr.Error())
		}
		return commandErrorResult(commandID, "execution_failed", fmt.Sprintf("terminalExec execution failed: %v", err))
	}

	resultPayload, err := json.Marshal(execResult)
	if err != nil {
		return commandErrorResult(commandID, "encode_failed", "failed to encode terminalExec payload")
	}

	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				PayloadJson:     resultPayload,
				CompletedUnixMs: time.Now().UnixMilli(),
			},
		},
	}
}

func buildTerminalResourceCommandResult(baseCtx context.Context, commandID string, dispatch *registryv1.CommandDispatch) *registryv1.ConnectRequest {
	payload := append([]byte(nil), dispatch.GetPayloadJson()...)
	if len(payload) == 0 {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "terminalResource payload is required")
	}

	decoded := terminalResourcePayload{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "payload_json is not valid terminalResource payload")
	}
	if strings.TrimSpace(decoded.SessionID) == "" || strings.TrimSpace(decoded.FilePath) == "" {
		return commandErrorResult(commandID, terminalExecCodeInvalidPayload, "terminalResource session_id and file_path are required")
	}

	commandCtx := baseCtx
	if commandCtx == nil {
		commandCtx = context.Background()
	}
	cancel := func() {}
	if deadlineUnixMS := dispatch.GetDeadlineUnixMs(); deadlineUnixMS > 0 {
		commandCtx, cancel = context.WithDeadline(commandCtx, time.UnixMilli(deadlineUnixMS))
	}
	defer cancel()

	resourceResult, err := runTerminalResource(commandCtx, terminalResourceRequest{
		SessionID: decoded.SessionID,
		FilePath:  decoded.FilePath,
		Action:    decoded.Action,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return commandErrorResult(commandID, "deadline_exceeded", "command deadline exceeded")
		}
		var terminalErr *terminalExecError
		if errors.As(err, &terminalErr) {
			return commandErrorResult(commandID, terminalErr.Code(), terminalErr.Error())
		}
		return commandErrorResult(commandID, "execution_failed", fmt.Sprintf("terminalResource execution failed: %v", err))
	}

	resultPayload, err := json.Marshal(resourceResult)
	if err != nil {
		return commandErrorResult(commandID, "encode_failed", "failed to encode terminalResource payload")
	}

	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				PayloadJson:     resultPayload,
				CompletedUnixMs: time.Now().UnixMilli(),
			},
		},
	}
}

type dockerCommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

type dockerContainerState struct {
	Status   string
	ExitCode int
}

func runPythonExecInDockerWithImage(ctx context.Context, dockerImage string, code string) (pythonExecRunResult, error) {
	containerName, err := pythonExecContainerNameFn()
	if err != nil {
		return pythonExecRunResult{}, fmt.Errorf("allocate pythonExec container name: %w", err)
	}

	createResult := runDockerCommand(ctx, pythonExecDockerCreateArgsWithImage(containerName, dockerImage, code)...)
	if createResult.Err != nil {
		return pythonExecRunResult{}, fmt.Errorf("docker create failed: %w", createResult.Err)
	}
	if createResult.ExitCode != 0 {
		return pythonExecRunResult{}, fmt.Errorf("docker create failed: %s", dockerCommandFailureMessage("exit code", createResult.ExitCode, createResult.Stderr))
	}

	defer cleanupPythonExecContainer(containerName)

	startResult := runDockerCommand(ctx, pythonExecDockerStartArgs(containerName)...)
	if startResult.Err != nil {
		if errors.Is(startResult.Err, context.DeadlineExceeded) || errors.Is(startResult.Err, context.Canceled) {
			return pythonExecRunResult{}, startResult.Err
		}
		return pythonExecRunResult{}, fmt.Errorf("docker start failed: %w", startResult.Err)
	}

	if startResult.ExitCode == 0 {
		return pythonExecRunResult{
			Output:   startResult.Stdout,
			Stderr:   startResult.Stderr,
			ExitCode: 0,
		}, nil
	}

	state, stateErr := inspectPythonExecContainerState(containerName)
	if stateErr != nil {
		return pythonExecRunResult{}, fmt.Errorf("docker start failed with exit code %d: inspect failed: %w", startResult.ExitCode, stateErr)
	}
	if !isTerminalPythonExecContainerState(state.Status) {
		return pythonExecRunResult{}, fmt.Errorf(
			"docker start failed: state=%s %s",
			state.Status,
			dockerCommandFailureMessage("exit code", startResult.ExitCode, startResult.Stderr),
		)
	}

	return pythonExecRunResult{
		Output:   startResult.Stdout,
		Stderr:   startResult.Stderr,
		ExitCode: state.ExitCode,
	}, nil
}

func runDockerCommandCLI(ctx context.Context, args ...string) dockerCommandResult {
	command := exec.CommandContext(ctx, "docker", args...)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return dockerCommandResult{
				Stdout: stdout.String(),
				Stderr: stderr.String(),
				Err:    err,
			}
		}

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return dockerCommandResult{
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				ExitCode: exitErr.ExitCode(),
			}
		}

		return dockerCommandResult{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
			Err:    err,
		}
	}

	return dockerCommandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}
}

func pythonExecDockerCreateArgs(containerName string, code string) []string {
	return pythonExecDockerCreateArgsWithImage(containerName, defaultPythonExecDockerImage, code)
}

func pythonExecDockerCreateArgsWithImage(containerName string, dockerImage string, code string) []string {
	resolvedDockerImage := strings.TrimSpace(dockerImage)
	if resolvedDockerImage == "" {
		resolvedDockerImage = defaultPythonExecDockerImage
	}

	return []string{
		"create",
		"--name", containerName,
		"--label", pythonExecManagedLabel,
		"--label", pythonExecCapabilityLabel,
		"--label", pythonExecRuntimeLabel,
		"--memory", defaultPythonExecMemoryLimit,
		"--cpus", defaultPythonExecCPULimit,
		"--pids-limit", strconv.Itoa(defaultPythonExecPidsLimit),
		resolvedDockerImage,
		"python",
		"-c",
		code,
	}
}

func pythonExecDockerStartArgs(containerName string) []string {
	return []string{"start", "-a", containerName}
}

func pythonExecDockerInspectArgs(containerName string) []string {
	return []string{
		"inspect",
		"-f",
		"{{.State.Status}}|{{.State.ExitCode}}",
		containerName,
	}
}

func pythonExecDockerRemoveArgs(containerName string) []string {
	return []string{"rm", "-f", containerName}
}

func inspectPythonExecContainerState(containerName string) (dockerContainerState, error) {
	inspectCtx, cancel := context.WithTimeout(context.Background(), pythonExecInspectTimeout)
	defer cancel()

	result := runDockerCommand(inspectCtx, pythonExecDockerInspectArgs(containerName)...)
	if result.Err != nil {
		return dockerContainerState{}, result.Err
	}
	if result.ExitCode != 0 {
		return dockerContainerState{}, errors.New(dockerCommandFailureMessage("exit code", result.ExitCode, result.Stderr))
	}

	parts := strings.Split(strings.TrimSpace(result.Stdout), "|")
	if len(parts) != 2 {
		return dockerContainerState{}, fmt.Errorf("unexpected docker inspect output: %q", strings.TrimSpace(result.Stdout))
	}

	exitCode, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return dockerContainerState{}, fmt.Errorf("invalid container exit code: %w", err)
	}

	return dockerContainerState{
		Status:   strings.TrimSpace(parts[0]),
		ExitCode: exitCode,
	}, nil
}

func cleanupPythonExecContainer(containerName string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), pythonExecCleanupTimeout)
	defer cancel()

	result := runDockerCommand(cleanupCtx, pythonExecDockerRemoveArgs(containerName)...)
	if result.Err != nil {
		log.Printf("pythonExec cleanup failed: container=%s err=%v", containerName, result.Err)
		return
	}
	if result.ExitCode != 0 && !isNoSuchContainerMessage(result.Stderr) {
		log.Printf(
			"pythonExec cleanup failed: container=%s %s",
			containerName,
			dockerCommandFailureMessage("exit code", result.ExitCode, result.Stderr),
		)
	}
}

func newPythonExecContainerName() (string, error) {
	suffix, err := randomHex(8)
	if err != nil {
		return "", err
	}
	return pythonExecContainerPrefix + suffix, nil
}

func dockerCommandFailureMessage(prefix string, value int, stderr string) string {
	message := fmt.Sprintf("%s=%d", prefix, value)
	if trimmed := strings.TrimSpace(stderr); trimmed != "" {
		message = message + ", stderr=" + trimmed
	}
	return message
}

func isTerminalPythonExecContainerState(state string) bool {
	normalized := strings.TrimSpace(strings.ToLower(state))
	return normalized == "exited" || normalized == "dead"
}

func isNoSuchContainerMessage(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "no such container")
}

func commandErrorResult(commandID string, code string, message string) *registryv1.ConnectRequest {
	return &registryv1.ConnectRequest{
		Payload: &registryv1.ConnectRequest_CommandResult{
			CommandResult: &registryv1.CommandResult{
				CommandId:       commandID,
				CompletedUnixMs: time.Now().UnixMilli(),
				Error: &registryv1.CommandError{
					Code:    code,
					Message: message,
				},
			},
		},
	}
}

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

func runTerminalExecUnavailable(context.Context, terminalExecRequest) (terminalExecRunResult, error) {
	return terminalExecRunResult{}, newTerminalExecError("execution_failed", terminalExecNotReadyMessage)
}

func runTerminalResourceUnavailable(context.Context, terminalResourceRequest) (terminalResourceRunResult, error) {
	return terminalResourceRunResult{}, newTerminalExecError("execution_failed", terminalExecNotReadyMessage)
}

func enqueueRequest(ctx context.Context, outbound chan<- *registryv1.ConnectRequest, req *registryv1.ConnectRequest) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case outbound <- req:
		return nil
	}
}

func reportSessionErr(errCh chan<- error, err error) {
	if err == nil {
		return
	}
	select {
	case errCh <- err:
	default:
	}
}

func recvWithTimeout(
	ctx context.Context,
	timeout time.Duration,
	recv func() (*registryv1.ConnectResponse, error),
) (*registryv1.ConnectResponse, error) {
	if timeout <= 0 {
		return recv()
	}

	type recvResult struct {
		resp *registryv1.ConnectResponse
		err  error
	}

	resultCh := make(chan recvResult, 1)
	go func() {
		resp, err := recv()
		resultCh <- recvResult{resp: resp, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, context.DeadlineExceeded
	case result := <-resultCh:
		return result.resp, result.err
	}
}

func durationFromServer(seconds int32, fallback time.Duration) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if fallback <= 0 {
		return 5 * time.Second
	}
	return fallback
}

func randomHex(byteLength int) (string, error) {
	if byteLength <= 0 {
		return "", errors.New("byte length must be positive")
	}

	raw := make([]byte, byteLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func jitterDuration(base time.Duration, jitterPct int) time.Duration {
	if base <= 0 {
		base = minHeartbeatInterval
	}
	if jitterPct <= 0 {
		return base
	}

	maxDelta := int64(base) * int64(jitterPct) / 100
	if maxDelta <= 0 {
		return base
	}

	random, err := rand.Int(rand.Reader, big.NewInt(maxDelta*2+1))
	if err != nil {
		return base
	}
	delta := random.Int64() - maxDelta
	jittered := base + time.Duration(delta)
	if jittered < minHeartbeatInterval {
		return minHeartbeatInterval
	}
	return jittered
}

func waitReconnectDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		delay = initialReconnectDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextReconnectDelay(current time.Duration) time.Duration {
	if current <= 0 {
		return initialReconnectDelay
	}
	next := current * 2
	if next > maxReconnectDelay {
		return maxReconnectDelay
	}
	return next
}
