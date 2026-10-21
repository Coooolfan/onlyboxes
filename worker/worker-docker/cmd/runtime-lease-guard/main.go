// runtime-lease-guard is a trusted Docker-side helper, not a Worker service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/onlyboxes/onlyboxes/worker/worker-docker/internal/runtimelease"
)

func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, "runtime lease guard unavailable")
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("runtime-lease-guard", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	mode := flags.String("mode", "serve", "")
	state := flags.String("state-dir", "/state/leases", "")
	socket := flags.String("socket", "/run/onlyboxes-lease/control.sock", "")
	worker := flags.String("worker-id", "", "")
	dockerSocket := flags.String("docker-socket", "/var/run/docker.sock", "")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 {
		return runtimelease.ErrUnavailable
	}
	if *mode == "control" {
		var request runtimelease.Request
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 4097))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&request) != nil || decoder.Decode(&extra) != io.EOF {
			return runtimelease.ErrConflict
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		var entry runtimelease.Entry
		var identity runtimelease.Identity
		var err error
		if request.Action == "ping" {
			identity, err = runtimelease.Readiness(ctx, *socket)
		} else {
			entry, err = runtimelease.Call(ctx, *socket, request)
		}
		if err != nil {
			code := "unavailable"
			if errors.Is(err, runtimelease.ErrNotFound) {
				code = "not_found"
			}
			if errors.Is(err, runtimelease.ErrConflict) {
				code = "conflict"
			}
			_ = json.NewEncoder(os.Stdout).Encode(runtimelease.Response{Error: code})
			return err
		}
		if request.Action == "ping" {
			return json.NewEncoder(os.Stdout).Encode(identity)
		}
		if request.Action == "forget" {
			return json.NewEncoder(os.Stdout).Encode(map[string]bool{"forgotten": true})
		}
		return json.NewEncoder(os.Stdout).Encode(entry)
	}
	if *mode != "serve" {
		return runtimelease.ErrUnavailable
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	backend, err := runtimelease.NewDocker(probe, *dockerSocket, *worker)
	cancel()
	if err != nil {
		return err
	}
	guard, err := runtimelease.Open(*state, *worker, backend)
	if err != nil {
		return err
	}
	defer guard.Close()
	listener, err := runtimelease.Listen(*socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	return guard.Serve(ctx, listener)
}
