package e2b

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	processv1 "github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b/process/v1"
	"github.com/onlyboxes/onlyboxes/worker/worker-bridge-e2b/internal/e2b/process/v1/processv1connect"
)

const (
	envdPort                    = 49983
	envdProcessCleanupTimeout   = 3 * time.Second
	envdProcessTagCleanupGrace  = 250 * time.Millisecond
	envdProcessCleanupRetryWait = 25 * time.Millisecond
)

type Config struct {
	APIKey                string
	APIURL                string
	Domain                string
	SandboxURL            string
	RequestTimeout        time.Duration
	RestrictPublicTraffic bool
}

type Client struct {
	apiKey                string
	apiURL                string
	domain                string
	sandboxURL            string
	controlHTTP           *http.Client
	sandboxHTTP           *http.Client
	restrictPublicTraffic bool
}

type Sandbox struct {
	ID                 string
	Domain             string
	EnvdVersion        string
	AccessToken        string
	TrafficAccessToken string
}

type SandboxInfo struct {
	ID          string            `json:"sandboxID"`
	State       string            `json:"state"`
	EnvdVersion string            `json:"envdVersion"`
	Metadata    map[string]string `json:"metadata"`
}

type CommandResult struct {
	Stdout          string
	Stderr          string
	ExitCode        int
	StdoutTruncated bool
	StderrTruncated bool
}

type File struct {
	Content  []byte
	MIMEType string
	Size     int64
}

type FileReader struct {
	Body     io.ReadCloser
	MIMEType string
	Size     int64
}

type apiError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

type HTTPError struct {
	StatusCode int
	Status     string
	Message    string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "E2B API request failed"
	}
	return fmt.Sprintf("E2B API returned %s: %s", e.Status, e.Message)
}

func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("E2B API key is required")
	}
	apiURL := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	if apiURL == "" {
		apiURL = "https://api.e2b.app"
	}
	domain := strings.TrimSpace(cfg.Domain)
	if domain == "" {
		domain = "e2b.app"
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 60 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	sandboxTransport := transport.Clone()
	sandboxTransport.DialContext = (&net.Dialer{
		Timeout:   requestTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	sandboxTransport.TLSHandshakeTimeout = requestTimeout
	sandboxTransport.ResponseHeaderTimeout = requestTimeout
	return &Client{
		apiKey:      strings.TrimSpace(cfg.APIKey),
		apiURL:      apiURL,
		domain:      domain,
		sandboxURL:  strings.TrimRight(strings.TrimSpace(cfg.SandboxURL), "/"),
		controlHTTP: &http.Client{Transport: transport.Clone(), Timeout: requestTimeout},
		// Envd connection setup and response headers use the configured request
		// timeout. Once headers arrive, streams use the console dispatch context.
		sandboxHTTP:           &http.Client{Transport: sandboxTransport},
		restrictPublicTraffic: cfg.RestrictPublicTraffic,
	}, nil
}

func (c *Client) Create(ctx context.Context, template string, timeoutSec int) (*Sandbox, error) {
	return c.CreateWithMetadata(ctx, template, timeoutSec, map[string]string{"onlyboxes.worker": "worker-bridge-e2b"})
}

func (c *Client) CreateWithMetadata(ctx context.Context, template string, timeoutSec int, metadata map[string]string) (*Sandbox, error) {
	template = strings.TrimSpace(template)
	if template == "" {
		return nil, errors.New("E2B template is required")
	}
	if timeoutSec <= 0 {
		return nil, errors.New("E2B sandbox timeout must be positive")
	}
	body := struct {
		TemplateID          string            `json:"templateID"`
		Timeout             int               `json:"timeout"`
		AutoPause           bool              `json:"autoPause"`
		Secure              bool              `json:"secure"`
		AllowInternetAccess bool              `json:"allow_internet_access"`
		Metadata            map[string]string `json:"metadata"`
		EnvVars             map[string]string `json:"envVars"`
		Network             map[string]bool   `json:"network,omitempty"`
	}{
		TemplateID:          template,
		Timeout:             timeoutSec,
		AutoPause:           false,
		Secure:              true,
		AllowInternetAccess: true,
		Metadata:            maps.Clone(metadata),
		EnvVars:             map[string]string{},
	}
	if c.restrictPublicTraffic {
		body.Network = map[string]bool{"allowPublicTraffic": false}
	}
	var response struct {
		SandboxID          string  `json:"sandboxID"`
		EnvdVersion        string  `json:"envdVersion"`
		EnvdAccessToken    string  `json:"envdAccessToken"`
		TrafficAccessToken string  `json:"trafficAccessToken"`
		Domain             *string `json:"domain"`
	}
	if err := c.controlJSON(ctx, http.MethodPost, "/sandboxes", body, &response); err != nil {
		return nil, err
	}
	domain := c.domain
	if response.Domain != nil && strings.TrimSpace(*response.Domain) != "" {
		domain = strings.TrimSpace(*response.Domain)
	}
	if strings.TrimSpace(response.SandboxID) == "" {
		return nil, errors.New("E2B create response did not include sandboxID")
	}
	return &Sandbox{
		ID:                 response.SandboxID,
		Domain:             domain,
		EnvdVersion:        response.EnvdVersion,
		AccessToken:        response.EnvdAccessToken,
		TrafficAccessToken: response.TrafficAccessToken,
	}, nil
}

func (c *Client) List(ctx context.Context, metadata map[string]string) ([]SandboxInfo, error) {
	query := url.Values{}
	if len(metadata) > 0 {
		keys := make([]string, 0, len(metadata))
		for key := range metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(metadata[key]))
		}
		query.Set("metadata", strings.Join(parts, "&"))
	}
	path := "/sandboxes"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response []SandboxInfo
	if err := c.controlJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response, nil
}

func (c *Client) Connect(ctx context.Context, sandboxID string, timeoutSec int) (*Sandbox, error) {
	if timeoutSec <= 0 {
		return nil, errors.New("E2B sandbox timeout must be positive")
	}
	var response struct {
		SandboxID          string  `json:"sandboxID"`
		EnvdVersion        string  `json:"envdVersion"`
		EnvdAccessToken    string  `json:"envdAccessToken"`
		TrafficAccessToken string  `json:"trafficAccessToken"`
		Domain             *string `json:"domain"`
	}
	path := "/sandboxes/" + url.PathEscape(strings.TrimSpace(sandboxID)) + "/connect"
	err := c.controlJSON(ctx, http.MethodPost, path, struct {
		Timeout int `json:"timeout"`
	}{Timeout: timeoutSec}, &response)
	if isHTTPStatus(err, http.StatusNotFound) {
		return nil, fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	domain := c.domain
	if response.Domain != nil && strings.TrimSpace(*response.Domain) != "" {
		domain = strings.TrimSpace(*response.Domain)
	}
	id := strings.TrimSpace(response.SandboxID)
	if id == "" {
		id = strings.TrimSpace(sandboxID)
	}
	return &Sandbox{
		ID:                 id,
		Domain:             domain,
		EnvdVersion:        response.EnvdVersion,
		AccessToken:        response.EnvdAccessToken,
		TrafficAccessToken: response.TrafficAccessToken,
	}, nil
}

func (sandbox *Sandbox) ProxyURL(port int) (string, string, error) {
	if sandbox == nil || strings.TrimSpace(sandbox.ID) == "" {
		return "", "", errors.New("sandbox is required")
	}
	if port < 1 || port > 65535 {
		return "", "", errors.New("sandbox port must be between 1 and 65535")
	}
	domain := strings.TrimSpace(sandbox.Domain)
	if domain == "" || strings.ContainsAny(domain, "/:@?#") {
		return "", "", errors.New("sandbox domain is invalid")
	}
	trafficToken := strings.TrimSpace(sandbox.TrafficAccessToken)
	if trafficToken == "" {
		return "", "", errors.New("sandbox traffic access token is required")
	}
	return "https://" + strconv.Itoa(port) + "-" + sandbox.ID + "." + domain, trafficToken, nil
}

func (c *Client) SetTimeout(ctx context.Context, sandboxID string, timeoutSec int) error {
	if timeoutSec <= 0 {
		return errors.New("E2B sandbox timeout must be positive")
	}
	path := "/sandboxes/" + url.PathEscape(strings.TrimSpace(sandboxID)) + "/timeout"
	err := c.controlJSON(ctx, http.MethodPost, path, struct {
		Timeout int `json:"timeout"`
	}{Timeout: timeoutSec}, nil)
	if isHTTPStatus(err, http.StatusNotFound) {
		return fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
	}
	return err
}

func (c *Client) Kill(ctx context.Context, sandboxID string) error {
	err := c.controlJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(strings.TrimSpace(sandboxID)), nil, nil)
	if isHTTPStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (c *Client) Run(
	ctx context.Context,
	sandbox *Sandbox,
	command string,
	maxOutputBytes int,
) (result CommandResult, runErr error) {
	if sandbox == nil || strings.TrimSpace(sandbox.ID) == "" {
		return CommandResult{}, errors.New("sandbox is required")
	}
	baseURL := c.sandboxBaseURL(sandbox)
	rpc := processv1connect.NewProcessClient(c.sandboxHTTP, baseURL, connect.WithProtoJSON())
	stdin := false
	randomID, err := randomID()
	if err != nil {
		return CommandResult{}, fmt.Errorf("generate process tag: %w", err)
	}
	tag := "onlyboxes-" + randomID
	req := connect.NewRequest(&processv1.StartRequest{
		Process: &processv1.ProcessConfig{
			Cmd:  "/bin/bash",
			Args: []string{"-l", "-c", command},
			Envs: map[string]string{},
		},
		Tag:   &tag,
		Stdin: &stdin,
	})
	c.applySandboxHeaders(req.Header(), sandbox)
	req.Header().Set("Keepalive-Ping-Interval", "50")
	stream, err := rpc.Start(ctx, req)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return CommandResult{}, fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
		}
		return CommandResult{}, fmt.Errorf("start E2B command: %w", err)
	}
	var pid uint32
	ended := false
	defer func() {
		if ended {
			return
		}
		if err := c.stopProcess(sandbox, pid, tag); err != nil {
			if runErr == nil {
				runErr = fmt.Errorf("stop incomplete E2B command: %w", err)
				return
			}
			runErr = fmt.Errorf("%w; stop incomplete E2B command: %v", runErr, err)
		}
	}()
	var stdout, stderr limitedBuffer
	stdout.limit = maxOutputBytes
	stderr.limit = maxOutputBytes
	for stream.Receive() {
		event := stream.Msg().GetEvent()
		if event == nil {
			continue
		}
		if start := event.GetStart(); start != nil {
			pid = start.GetPid()
		}
		if data := event.GetData(); data != nil {
			stdout.Write(data.GetStdout())
			stderr.Write(data.GetStderr())
		}
		if end := event.GetEnd(); end != nil {
			ended = true
			result.ExitCode = int(end.GetExitCode())
			if end.GetError() != "" && result.ExitCode == 0 {
				return CommandResult{}, fmt.Errorf("E2B command failed: %s", end.GetError())
			}
		}
	}
	if err := stream.Err(); err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return CommandResult{}, fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
		}
		return CommandResult{}, fmt.Errorf("receive E2B command output: %w", err)
	}
	if !ended {
		return CommandResult{}, errors.New("E2B command ended without an end event")
	}
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	result.StdoutTruncated = stdout.truncated
	result.StderrTruncated = stderr.truncated
	return result, nil
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (c *Client) stopProcess(sandbox *Sandbox, pid uint32, tag string) error {
	ctx, cancel := context.WithTimeout(context.Background(), envdProcessCleanupTimeout)
	defer cancel()

	rpc := processv1connect.NewProcessClient(
		c.sandboxHTTP,
		c.sandboxBaseURL(sandbox),
		connect.WithProtoJSON(),
	)
	selector := &processv1.ProcessSelector{}
	if pid != 0 {
		selector.Selector = &processv1.ProcessSelector_Pid{Pid: pid}
	} else {
		selector.Selector = &processv1.ProcessSelector_Tag{Tag: tag}
	}
	req := connect.NewRequest(&processv1.SendSignalRequest{
		Process: selector,
		Signal:  processv1.Signal_SIGNAL_SIGKILL,
	})
	c.applySandboxHeaders(req.Header(), sandbox)

	tagDeadline := time.Now().Add(envdProcessTagCleanupGrace)
	for {
		_, err := rpc.SendSignal(ctx, req)
		if err == nil || (pid != 0 && connect.CodeOf(err) == connect.CodeNotFound) {
			return nil
		}
		if pid != 0 || connect.CodeOf(err) != connect.CodeNotFound {
			return err
		}
		if !time.Now().Before(tagDeadline) {
			return nil
		}
		timer := time.NewTimer(envdProcessCleanupRetryWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) ReadFile(ctx context.Context, sandbox *Sandbox, filePath string, maxBytes int64) (File, error) {
	opened, err := c.OpenFile(ctx, sandbox, filePath)
	if err != nil {
		return File{}, err
	}
	defer opened.Body.Close()
	if maxBytes >= 0 && opened.Size > maxBytes {
		return File{Size: opened.Size}, ErrFileTooLarge
	}
	reader := io.Reader(opened.Body)
	if maxBytes >= 0 {
		reader = io.LimitReader(opened.Body, maxBytes+1)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		return File{}, fmt.Errorf("read E2B file: %w", err)
	}
	if maxBytes >= 0 && int64(len(content)) > maxBytes {
		return File{Size: int64(len(content))}, ErrFileTooLarge
	}
	mimeType := opened.MIMEType
	if mimeType == "" {
		mimeType = http.DetectContentType(content)
	}
	return File{Content: content, MIMEType: mimeType, Size: int64(len(content))}, nil
}

func (c *Client) OpenFile(ctx context.Context, sandbox *Sandbox, filePath string) (FileReader, error) {
	if sandbox == nil || strings.TrimSpace(sandbox.ID) == "" {
		return FileReader{}, errors.New("sandbox is required")
	}
	endpoint := c.sandboxBaseURL(sandbox) + "/files?path=" + url.QueryEscape(filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return FileReader{}, err
	}
	c.applySandboxHeaders(req.Header, sandbox)
	resp, err := c.sandboxHTTP.Do(req)
	if err != nil {
		return FileReader{}, fmt.Errorf("download E2B file: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return FileReader{}, ErrFileNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := responseError(resp)
		resp.Body.Close()
		return FileReader{}, err
	}
	mimeType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = mime.TypeByExtension(filepath.Ext(filePath))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return FileReader{Body: resp.Body, MIMEType: mimeType, Size: resp.ContentLength}, nil
}

var (
	ErrFileNotFound    = errors.New("file not found")
	ErrFileTooLarge    = errors.New("file exceeds limit")
	ErrSandboxNotFound = errors.New("sandbox not found")
)

func (c *Client) controlJSON(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("User-Agent", "onlyboxes-worker-bridge-e2b")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.controlHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("E2B API request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	if output == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
		return fmt.Errorf("decode E2B API response: %w", err)
	}
	return nil
}

func responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var decoded apiError
	_ = json.Unmarshal(body, &decoded)
	message := strings.TrimSpace(decoded.Message)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = resp.Status
	}
	return &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Message:    message,
	}
}

func isHTTPStatus(err error, statusCode int) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == statusCode
}

func (c *Client) sandboxBaseURL(sandbox *Sandbox) string {
	if c.sandboxURL != "" {
		return c.sandboxURL
	}
	domain := strings.TrimSpace(sandbox.Domain)
	if domain == "" {
		domain = c.domain
	}
	if domain == "e2b.app" {
		return "https://sandbox.e2b.app"
	}
	return "https://" + strconv.Itoa(envdPort) + "-" + sandbox.ID + "." + domain
}

func (c *Client) applySandboxHeaders(header http.Header, sandbox *Sandbox) {
	header.Set("E2b-Sandbox-Id", sandbox.ID)
	header.Set("E2b-Sandbox-Port", strconv.Itoa(envdPort))
	header.Set("User-Agent", "onlyboxes-worker-bridge-e2b")
	if sandbox.AccessToken != "" {
		header.Set("X-Access-Token", sandbox.AccessToken)
	}
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(data []byte) {
	if len(data) == 0 {
		return
	}
	if b.limit < 0 {
		_, _ = b.buf.Write(data)
		return
	}
	if b.limit == 0 {
		b.truncated = true
		return
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return
	}
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(data)
}

func (b *limitedBuffer) String() string { return b.buf.String() }
