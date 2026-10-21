package runtimelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

type Docker struct {
	client         *http.Client
	prefix, worker string
}

// NewDocker accepts a local Unix socket only. It never follows HTTP redirects,
// reads Docker CLI credentials, or sends daemon diagnostics to the caller.
func NewDocker(ctx context.Context, socket, worker string) (*Docker, error) {
	if !filepath.IsAbs(socket) || worker == "" {
		return nil, ErrUnavailable
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, MaxIdleConnsPerHost: 8, ResponseHeaderTimeout: 5 * time.Second}
	d := &Docker{worker: worker, client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	var version struct {
		APIVersion string `json:"ApiVersion"`
	}
	response, err := d.request(ctx, http.MethodGet, "/version")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&version) != nil || !regexp.MustCompile(`^1\.[0-9]{1,3}$`).MatchString(version.APIVersion) {
		return nil, ErrUnavailable
	}
	minor, err := strconv.Atoi(version.APIVersion[2:])
	if err != nil || minor < 42 {
		return nil, ErrUnavailable
	}
	d.prefix = "/v" + version.APIVersion
	return d, nil
}

func (d *Docker) request(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+d.prefix+path, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	r, err := d.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	return r, nil
}

func (d *Docker) Inspect(ctx context.Context, e Entry) (bool, bool, error) {
	if !e.valid() {
		return false, false, ErrConflict
	}
	r, err := d.request(ctx, http.MethodGet, "/containers/"+e.ContainerID+"/json")
	if err != nil {
		return false, false, err
	}
	defer r.Body.Close()
	if r.StatusCode == 404 {
		return false, false, nil
	}
	if r.StatusCode != 200 {
		return false, false, ErrUnavailable
	}
	var c struct {
		ID         string `json:"Id"`
		State      struct{ Running bool }
		Config     struct{ Labels map[string]string }
		HostConfig struct{ RestartPolicy struct{ Name string } }
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&c) != nil {
		return false, false, ErrUnavailable
	}
	hash := sha256.Sum256([]byte(e.Scope))
	labels := c.Config.Labels
	nativeGeneration := int64(1)
	if raw := labels["onlyboxes.generation"]; raw != "" {
		var parseErr error
		nativeGeneration, parseErr = strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			return false, false, ErrConflict
		}
	}
	if nativeGeneration != e.Generation {
		return false, false, ErrConflict
	}
	if c.ID != e.ContainerID || labels["onlyboxes.managed"] != "true" || labels["onlyboxes.worker_id"] != d.worker || labels["onlyboxes.session_id_hash"] != hex.EncodeToString(hash[:]) || labels["onlyboxes.capability"] != "terminalExec" || (c.HostConfig.RestartPolicy.Name != "" && c.HostConfig.RestartPolicy.Name != "no") {
		return false, false, ErrConflict
	}
	return true, c.State.Running, nil
}

func (d *Docker) Stop(ctx context.Context, e Entry) error {
	present, running, err := d.Inspect(ctx, e)
	if err != nil {
		return err
	}
	if !present || !running {
		return nil
	}
	r, err := d.request(ctx, http.MethodPost, "/containers/"+e.ContainerID+"/stop?t=0&signal=SIGKILL")
	if err != nil {
		return err
	}
	r.Body.Close()
	if r.StatusCode != 204 && r.StatusCode != 304 && r.StatusCode != 404 {
		return ErrUnavailable
	}
	_, running, err = d.Inspect(ctx, e)
	if err != nil || running {
		return ErrUnavailable
	}
	return nil
}
