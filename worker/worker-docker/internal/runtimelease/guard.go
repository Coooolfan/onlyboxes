package runtimelease

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// Backend must address only the full immutable ID and validate Worker/scope
// ownership. Stop never removes a rootfs or starts/unpauses a container itself.
type Backend interface {
	Inspect(context.Context, Entry) (exists, running bool, err error)
	Stop(context.Context, Entry) error
}

type Request struct {
	Action           string `json:"action"`
	ExpectedRevision int64  `json:"expected_revision"`
	Entry            Entry  `json:"entry"`
}

type Guard struct {
	mu        sync.Mutex
	store     *store
	backend   Backend
	entries   map[string]Entry
	locks     [256]sync.Mutex
	deadlines map[string]time.Time
	poisoned  bool
	wall      func() int64
	mono      func() time.Time
}

func Open(root, worker string, backend Backend) (*Guard, error) {
	if backend == nil {
		return nil, ErrUnavailable
	}
	s, entries, err := openStore(root, worker)
	if err != nil {
		return nil, err
	}
	g := &Guard{store: s, backend: backend, entries: entries, deadlines: map[string]time.Time{}, wall: func() int64 { return time.Now().UnixMilli() }, mono: time.Now}
	// A restarted guardian cannot prove continuity of its monotonic clocks.
	// Revoke every prior lease before accepting any control or returning ready.
	for id, e := range entries {
		if !e.Revoked {
			e.Revoked = true
			e.Revision++
			if !e.valid() || s.write(id+".json", e) != nil {
				s.close()
				return nil, ErrUnavailable
			}
			entries[id] = e
		}
	}
	return g, nil
}

func (g *Guard) lockID(id string) *sync.Mutex {
	bucket, _ := strconv.ParseUint(id[:2], 16, 8)
	return &g.locks[bucket]
}

func (g *Guard) Status(id string) (Entry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.poisoned {
		return Entry{}, ErrUnavailable
	}
	e, ok := g.entries[id]
	if !ok || (!e.Revoked && g.expired(e)) {
		return Entry{}, ErrConflict
	}
	return e, nil
}

// Apply is a compare-and-swap. Replaying the exact accepted request is safe;
// any older revision or different payload is rejected. Renew cannot revive an
// expired lease. Only explicit arm can authorize another controlled boot.
func (g *Guard) Apply(ctx context.Context, r Request) (Entry, error) {
	e := r.Entry
	if !e.valid() || r.ExpectedRevision < 0 || e.Revision != r.ExpectedRevision+1 || e.Revoked || (r.Action != "arm" && r.Action != "renew" && r.Action != "revoke") {
		return Entry{}, ErrConflict
	}
	l := g.lockID(e.ContainerID)
	l.Lock()
	defer l.Unlock()
	if r.Action == "revoke" {
		e.Revoked = true
	}
	g.mu.Lock()
	old, exists, replay, err := g.validate(r, e)
	deadline := g.mono().Add(time.Duration(e.ExpiresUnixMS-g.wall()) * time.Millisecond)
	if exists && !old.Revoked && !g.expired(old) {
		if prior, ok := g.deadlines[e.ContainerID]; ok {
			bounded := prior.Add(time.Duration(e.ExpiresUnixMS-old.ExpiresUnixMS) * time.Millisecond)
			if bounded.Before(deadline) {
				deadline = bounded
			}
		}
	}
	needsStop := exists && (old.Revoked || g.expired(old))
	g.mu.Unlock()
	if err != nil {
		return Entry{}, err
	}
	if replay {
		return old, nil
	}
	if r.Action != "revoke" {
		if needsStop {
			if g.backend.Stop(ctx, old) != nil {
				return Entry{}, ErrUnavailable
			}
		}
		present, _, err := g.backend.Inspect(ctx, e)
		if err != nil || !present {
			return Entry{}, ErrUnavailable
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, _, _, err := g.validate(r, e); err != nil {
		return Entry{}, err
	}
	if !e.Revoked && !g.mono().Before(deadline) {
		return Entry{}, ErrConflict
	}
	if g.store.write(e.ContainerID+".json", e) != nil {
		g.poisoned = true
		g.entries[e.ContainerID] = e
		return Entry{}, ErrUnavailable
	}
	g.entries[e.ContainerID] = e
	g.deadlines[e.ContainerID] = deadline
	return e, nil
}

// validate requires mu. Native I/O never holds the global mutex, so a stalled
// control for one CID cannot postpone unrelated expiry enforcement.
func (g *Guard) validate(r Request, e Entry) (Entry, bool, bool, error) {
	if g.poisoned {
		return Entry{}, false, false, ErrUnavailable
	}
	old, exists := g.entries[e.ContainerID]
	if exists && e == old {
		if !e.Revoked && g.expired(old) {
			return old, exists, false, ErrConflict
		}
		return old, exists, true, nil
	}
	if (!exists && (r.ExpectedRevision != 0 || r.Action != "arm" || len(g.entries) >= maxEntries)) || (exists && (old.Revision != r.ExpectedRevision || old.Scope != e.Scope || old.Generation != e.Generation)) {
		return old, exists, false, ErrConflict
	}
	if r.Action != "revoke" {
		remaining := e.ExpiresUnixMS - g.wall()
		if remaining <= 0 || remaining > int64((1<<63-1)/time.Millisecond) {
			return old, exists, false, ErrConflict
		}
		if exists && (e.ExpiresUnixMS < old.ExpiresUnixMS || (r.Action == "renew" && (old.Revoked || old.Generation != e.Generation || g.expired(old)))) {
			return old, exists, false, ErrConflict
		}
		for id, other := range g.entries {
			if id != e.ContainerID && other.Scope == e.Scope {
				return old, exists, false, ErrConflict
			}
		}
	}
	return old, exists, false, nil
}

func (g *Guard) expired(e Entry) bool {
	d, ok := g.deadlines[e.ContainerID]
	return e.Revoked || e.ExpiresUnixMS <= g.wall() || !ok || !g.mono().Before(d)
}

// Forget releases capacity only after the immutable native ID no longer
// exists. A stopped but retained writable layer still owns its lease record.
func (g *Guard) Forget(ctx context.Context, id string, revision int64) error {
	if !cidPattern.MatchString(id) || revision <= 0 {
		return ErrConflict
	}
	l := g.lockID(id)
	l.Lock()
	defer l.Unlock()
	g.mu.Lock()
	e, ok := g.entries[id]
	poisoned := g.poisoned
	g.mu.Unlock()
	if poisoned {
		return ErrUnavailable
	}
	if !ok {
		return nil
	}
	if e.Revision != revision {
		return ErrConflict
	}
	present, _, err := g.backend.Inspect(ctx, e)
	if err != nil {
		return ErrUnavailable
	}
	if present {
		return ErrConflict
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.poisoned {
		return ErrUnavailable
	}
	if g.store.remove(id+".json") != nil {
		g.poisoned = true
		return ErrUnavailable
	}
	delete(g.entries, id)
	delete(g.deadlines, id)
	return nil
}

func (g *Guard) revokeAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, e := range g.entries {
		if !e.Revoked {
			e.Revoked = true
			e.Revision++
			if !e.valid() || g.store.write(id+".json", e) != nil {
				g.poisoned = true
			}
			g.entries[id] = e
		}
	}
}

func (g *Guard) enforce(ctx context.Context, id string) error {
	l := g.lockID(id)
	l.Lock()
	defer l.Unlock()
	g.mu.Lock()
	e, ok := g.entries[id]
	if !ok || (!g.poisoned && !g.expired(e)) {
		g.mu.Unlock()
		return nil
	}
	if !e.Revoked {
		e.Revoked = true
		e.Revision++
		if !e.valid() || g.store.write(id+".json", e) != nil {
			g.poisoned = true
		}
		g.entries[id] = e
	}
	g.mu.Unlock()
	// Keep stopping revoked IDs on later ticks: a delayed start request must
	// not turn a timed-out operation into an unleased writer.
	if err := g.backend.Stop(ctx, e); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Tick uses bounded parallel native I/O. One unresponsive container cannot
// indefinitely delay enforcement for every other session on the Worker.
func (g *Guard) Tick(ctx context.Context) error {
	g.mu.Lock()
	ids := make([]string, 0, len(g.entries))
	for id := range g.entries {
		ids = append(ids, id)
	}
	g.mu.Unlock()
	jobs := make(chan string)
	var wg sync.WaitGroup
	var failed bool
	var resultMu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				call, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := g.enforce(call, id)
				cancel()
				if err != nil {
					resultMu.Lock()
					failed = true
					resultMu.Unlock()
				}
			}
		}()
	}
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ErrUnavailable
		}
	}
	close(jobs)
	wg.Wait()
	if failed {
		return ErrUnavailable
	}
	return nil
}

// Close requires the caller to stop admission and join Tick/Apply first.
func (g *Guard) Close() { g.store.close() }
