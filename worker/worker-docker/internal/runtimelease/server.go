package runtimelease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Identity struct {
	StateID  string `json:"state_id"`
	Ready    bool   `json:"ready"`
	WorkerID string `json:"worker_id"`
	EngineID string `json:"engine_id"`
}

type Response struct {
	Identity  *Identity `json:"identity,omitempty"`
	Ready     bool      `json:"ready,omitempty"`
	Forgotten bool      `json:"forgotten,omitempty"`
	Entry     *Entry    `json:"entry,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Serve exposes controls only through a private Unix socket. The caller owns
// the listener and must not close the journal until Serve has joined Tick.
func (g *Guard) Serve(ctx context.Context, listener net.Listener) error {
	slots := make(chan struct{}, 16)
	loop, stop := context.WithCancel(ctx)
	defer stop()
	var handlerMu sync.Mutex
	var handlers sync.WaitGroup
	closing := false
	server := &http.Server{ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 4096}
	server.BaseContext = func(net.Listener) context.Context { return loop }
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerMu.Lock()
		if closing {
			handlerMu.Unlock()
			http.Error(w, "runtime lease unavailable", 503)
			return
		}
		handlers.Add(1)
		handlerMu.Unlock()
		defer handlers.Done()
		if r.Method != http.MethodPost || r.URL.Path != "/control" {
			http.NotFound(w, r)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "runtime lease unavailable", 503)
			return
		}
		var request Request
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&request) != nil || decoder.Decode(&extra) != io.EOF {
			http.Error(w, "invalid control", 400)
			return
		}
		call, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var entry Entry
		var err error
		if request.Action == "ping" {
			g.mu.Lock()
			if g.poisoned {
				err = ErrUnavailable
			}
			g.mu.Unlock()
			if request.Entry != (Entry{}) || request.ExpectedRevision != 0 {
				err = ErrConflict
			}
		} else if request.Action == "forget" {
			err = g.Forget(call, request.Entry.ContainerID, request.ExpectedRevision)
			entry = request.Entry
		} else if request.Action == "status" {
			entry, err = g.Status(request.Entry.ContainerID)
		} else {
			entry, err = g.Apply(call, request)
		}
		response := Response{Entry: &entry}
		if request.Action == "ping" && err == nil {
			response.Entry = nil
			response.Ready = true
			identity := Identity{Ready: true, WorkerID: g.store.worker, StateID: g.store.stateID}
			if backend, ok := g.backend.(interface{ EngineID() string }); ok {
				identity.EngineID = backend.EngineID()
			}
			response.Identity = &identity
		}
		if request.Action == "forget" && err == nil {
			response.Entry = nil
			response.Forgotten = true
		}
		if err != nil {
			response.Entry = nil
			response.Error = "conflict"
			if errors.Is(err, ErrUnavailable) {
				response.Error = "unavailable"
			}
			if errors.Is(err, ErrNotFound) {
				response.Error = "not_found"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			_ = g.Tick(loop)
			select {
			case <-loop.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-serveDone:
	}
	handlerMu.Lock()
	closing = true
	handlerMu.Unlock()
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	_ = server.Close()
	handlers.Wait()
	<-done
	g.revokeAll()
	_ = g.Tick(shutdown)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return ErrUnavailable
	}
	return nil
}

func Listen(socket string) (net.Listener, error) {
	if !filepath.IsAbs(socket) {
		return nil, ErrUnavailable
	}
	parent := filepath.Dir(socket)
	if os.MkdirAll(parent, 0700) != nil {
		return nil, ErrUnavailable
	}
	st, err := os.Lstat(parent)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, ErrUnavailable
	}
	if st, err = os.Lstat(socket); err == nil {
		if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm()&0077 != 0 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			return nil, ErrUnavailable
		}
		if connection, dialErr := net.DialTimeout("unix", socket, 100*time.Millisecond); dialErr == nil {
			connection.Close()
			return nil, ErrUnavailable
		}
		if os.Remove(socket) != nil {
			return nil, ErrUnavailable
		}
	} else if !os.IsNotExist(err) {
		return nil, ErrUnavailable
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, ErrUnavailable
	}
	if os.Chmod(socket, 0600) != nil {
		l.Close()
		return nil, ErrUnavailable
	}
	return l, nil
}

func callResponse(ctx context.Context, socket string, request Request) (Response, error) {
	if !filepath.IsAbs(socket) {
		return Response{}, ErrUnavailable
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > 4096 {
		return Response{}, ErrConflict
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 6 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guard/control", bytes.NewReader(data))
	if err != nil {
		return Response{}, ErrUnavailable
	}
	r, err := client.Do(req)
	if err != nil {
		return Response{}, ErrUnavailable
	}
	defer r.Body.Close()
	var response Response
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4097))
	decoder.DisallowUnknownFields()
	var extra any
	if r.StatusCode != 200 || decoder.Decode(&response) != nil || decoder.Decode(&extra) != io.EOF {
		return Response{}, ErrUnavailable
	}
	if response.Error != "" {
		if response.Entry != nil || response.Identity != nil || response.Ready || response.Forgotten {
			return Response{}, ErrUnavailable
		}
		if response.Error == "not_found" {
			return Response{}, ErrNotFound
		}
		if response.Error == "conflict" {
			return Response{}, ErrConflict
		}
		return Response{}, ErrUnavailable
	}
	return response, nil
}

func Readiness(ctx context.Context, socket string) (Identity, error) {
	response, err := callResponse(ctx, socket, Request{Action: "ping"})
	if err != nil {
		return Identity{}, err
	}
	if !response.Ready || response.Forgotten || response.Entry != nil || response.Identity == nil || !response.Identity.Ready || response.Identity.WorkerID == "" || !validEngineID(response.Identity.EngineID) || !stateIDPattern.MatchString(response.Identity.StateID) {
		return Identity{}, ErrUnavailable
	}
	return *response.Identity, nil
}

func Call(ctx context.Context, socket string, request Request) (Entry, error) {
	response, err := callResponse(ctx, socket, request)
	if err != nil {
		return Entry{}, err
	}
	if request.Action == "ping" && response.Ready && response.Entry == nil && !response.Forgotten {
		return Entry{}, nil
	}
	if response.Ready {
		return Entry{}, ErrUnavailable
	}
	if request.Action == "forget" && response.Forgotten && response.Entry == nil {
		return Entry{}, nil
	}
	if response.Forgotten || response.Entry == nil || !response.Entry.valid() || response.Entry.ContainerID != request.Entry.ContainerID {
		return Entry{}, ErrUnavailable
	}
	if request.Action == "arm" || request.Action == "renew" || request.Action == "revoke" {
		want := request.Entry
		if request.Action == "revoke" {
			want.Revoked = true
		}
		if want != *response.Entry {
			return Entry{}, ErrUnavailable
		}
	}
	if !response.Entry.Revoked && response.Entry.ExpiresUnixMS <= time.Now().UnixMilli() {
		return Entry{}, ErrConflict
	}
	return *response.Entry, nil
}
