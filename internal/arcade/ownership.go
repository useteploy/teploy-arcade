package arcade

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// Panel-owned work has one cancellation boundary. Closing a panel detaches
// Docker supervision; game containers intentionally survive a panel restart.
func (m *Manager) own(fn func()) bool {
	m.workerMu.Lock()
	defer m.workerMu.Unlock()
	if m.closing {
		return false
	}
	m.workers.Add(1)
	go func() { defer m.workers.Done(); fn() }()
	return true
}

func (m *Manager) Close() {
	m.workerMu.Lock()
	m.closing = true
	m.cancel()
	m.workerMu.Unlock()
	for _, s := range m.List() {
		s.mu.Lock()
		if s.proc != nil {
			s.proc.stop()
		}
		s.mu.Unlock()
	}
	m.workers.Wait()
}

func waitContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Only an authoritative observation permits an operation that assumes no
// external writer. Unknown includes transport outages, permissions and timeout.
func (m *Manager) requireStoppedContainer(s *Server) error {
	if s.Runtime != RuntimeDocker {
		return nil
	}
	switch containerState(s.ID) {
	case ContainerStopped, ContainerMissing:
		return nil
	case ContainerRunning:
		return fmt.Errorf("this server's container is still running; stop it before snapshot or restore")
	default:
		return fmt.Errorf("Docker container state is unknown; snapshot and restore are blocked until the daemon confirms it is stopped or missing")
	}
}

// Serialize space-consuming transactions on each filesystem. Holding admission
// through publication is a full-capacity lease: concurrent panel operations
// cannot spend the same observed free bytes. External writers remain outside
// panel control; per-entry free checks continue to protect the reserve margin.
var capacityGates = struct {
	sync.Mutex
	byDevice map[uint64]chan struct{}
}{byDevice: map[uint64]chan struct{}{}}

func acquireCapacity(ctx context.Context, path string) (func(), error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("cannot identify destination filesystem")
	}
	key := uint64(st.Dev)
	capacityGates.Lock()
	gate := capacityGates.byDevice[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		capacityGates.byDevice[key] = gate
	}
	capacityGates.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) markRecovery(s *Server, reason string) {
	s.mu.Lock()
	s.RecoveryRequired = reason
	s.mu.Unlock()
	_ = m.Save()
}

func (m *Manager) beginWork() (func(), error) {
	m.workerMu.Lock()
	defer m.workerMu.Unlock()
	if m.closing || m.ctx == nil || m.ctx.Err() != nil {
		return nil, context.Canceled
	}
	m.workers.Add(1)
	return m.workers.Done, nil
}

func (m *Manager) markRecoveryHeld(s *Server, reason string) {
	s.mu.Lock()
	s.RecoveryRequired = reason
	s.mu.Unlock()
	_ = m.saveHeld()
}
