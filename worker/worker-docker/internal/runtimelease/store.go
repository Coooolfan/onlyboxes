// Package runtimelease enforces execution leases outside the Worker process.
package runtimelease

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var (
	ErrNotFound    = errors.New("runtime lease not found")
	ErrConflict    = errors.New("runtime lease conflict")
	ErrUnavailable = errors.New("runtime lease unavailable")
	stateIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	cidPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const maxEntries = 16384

// Entry contains no execution environment, registry secret or storage grant.
type Entry struct {
	ContainerID   string `json:"container_id"`
	Scope         string `json:"scope"`
	Generation    int64  `json:"generation"`
	Revision      int64  `json:"revision"`
	ExpiresUnixMS int64  `json:"expires_unix_ms"`
	Revoked       bool   `json:"revoked"`
}

func (e Entry) valid() bool {
	parts := strings.SplitN(e.Scope, ":", 3)
	return len(parts) == 3 && parts[0] == "obx" && parts[1] != "" && parts[2] != "" && strings.TrimSpace(e.Scope) == e.Scope && cidPattern.MatchString(e.ContainerID) && len(e.Scope) > 4 && len(e.Scope) <= 1024 && bytes.HasPrefix([]byte(e.Scope), []byte("obx:")) && !bytes.ContainsAny([]byte(e.Scope), "\x00\r\n") && e.Generation > 0 && e.Revision > 0 && e.Revision < 1<<62 && e.ExpiresUnixMS > 0
}

type storeOwner struct {
	Version int    `json:"version"`
	Worker  string `json:"worker"`
	StateID string `json:"state_id"`
}

type store struct {
	stateID string
	root    string
	lock    *os.File
	worker  string
}

func openStore(root, worker string) (*store, map[string]Entry, error) {
	if !filepath.IsAbs(root) || worker == "" || len(worker) > 128 {
		return nil, nil, ErrUnavailable
	}
	if os.MkdirAll(root, 0700) != nil {
		return nil, nil, ErrUnavailable
	}
	st, err := os.Lstat(root)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, nil, ErrUnavailable
	}
	f, err := os.OpenFile(filepath.Join(root, "guard.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, nil, ErrUnavailable
	}
	lockInfo, lockErr := f.Stat()
	if lockErr != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm()&0077 != 0 || lockInfo.Sys().(*syscall.Stat_t).Nlink != 1 || lockInfo.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, nil, ErrUnavailable
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, nil, ErrUnavailable
	}
	s := &store{root: root, lock: f, worker: worker}
	fail := func() (*store, map[string]Entry, error) { s.close(); return nil, nil, ErrUnavailable }
	files, err := os.ReadDir(root)
	if err != nil || len(files) > maxEntries+64 {
		return fail()
	}
	entries := make(map[string]Entry)
	ownerFound := false
	pending := []string{}
	for _, file := range files {
		name := file.Name()
		if strings.HasPrefix(name, ".pending-") {
			st, err := os.Lstat(filepath.Join(root, name))
			if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 4096 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || st.Sys().(*syscall.Stat_t).Nlink != 1 {
				return fail()
			}
			pending = append(pending, name)
			continue
		}
		if name == "guard.lock" {
			continue
		}
		if name == "owner.json" {
			var owner storeOwner
			if s.read(name, &owner) != nil || owner.Version != 2 || owner.Worker != worker || !stateIDPattern.MatchString(owner.StateID) {
				return fail()
			}
			s.stateID = owner.StateID
			ownerFound = true
			continue
		}
		if len(name) != 69 || name[64:] != ".json" || !cidPattern.MatchString(name[:64]) {
			return fail()
		}
		var e Entry
		if s.read(name, &e) != nil || !e.valid() || e.ContainerID != name[:64] {
			return fail()
		}
		entries[e.ContainerID] = e
	}
	if len(entries) > maxEntries {
		return fail()
	}
	if !ownerFound {
		if len(entries) != 0 || len(pending) != 0 {
			return fail()
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return fail()
		}
		s.stateID = hex.EncodeToString(random[:])
		if s.write("owner.json", storeOwner{Version: 2, Worker: worker, StateID: s.stateID}) != nil {
			return fail()
		}
	}
	for _, name := range pending {
		if s.remove(name) != nil {
			return fail()
		}
	}
	return s, entries, nil
}

func (s *store) read(name string, value any) error {
	f, err := os.OpenFile(filepath.Join(s.root, name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ErrUnavailable
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 4096 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || st.Sys().(*syscall.Stat_t).Nlink != 1 {
		return ErrUnavailable
	}
	d := json.NewDecoder(io.LimitReader(f, 4097))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		return ErrUnavailable
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrUnavailable
	}
	return nil
}

func (s *store) write(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 4096 {
		return ErrUnavailable
	}
	f, err := os.CreateTemp(s.root, ".pending-")
	if err != nil {
		return ErrUnavailable
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || os.Rename(tmp, filepath.Join(s.root, name)) != nil {
		return ErrUnavailable
	}
	d, err := os.Open(s.root)
	if err != nil {
		return ErrUnavailable
	}
	defer d.Close()
	if d.Sync() != nil {
		return ErrUnavailable
	}
	return nil
}

func (s *store) close() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}

func (s *store) remove(name string) error {
	if err := os.Remove(filepath.Join(s.root, name)); err != nil && !os.IsNotExist(err) {
		return ErrUnavailable
	}
	d, err := os.Open(s.root)
	if err != nil {
		return ErrUnavailable
	}
	defer d.Close()
	if d.Sync() != nil {
		return ErrUnavailable
	}
	return nil
}
