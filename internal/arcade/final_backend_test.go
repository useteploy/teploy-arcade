package arcade

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFinalUnknownDockerCannotSnapshotOrRestore(t *testing.T) {
	_, m := newTestAgent(t)
	defer m.Close()
	s := m.List()[0]
	if err := m.WriteFile(s, "world/level.dat", "original"); err != nil {
		t.Fatal(err)
	}
	b, err := m.CreateBackup(s, "fixture", "test")
	if err != nil {
		t.Fatal(err)
	}
	s.Runtime = RuntimeDocker
	old := containerState
	defer func() { containerState = old }()
	for _, state := range []ContainerState{ContainerUnknown, ContainerRunning, ContainerState(99)} {
		containerState = func(string) ContainerState { return state }
		if _, err := m.CreateBackup(s, "must refuse", "test"); err == nil {
			t.Fatal("snapshot accepted unproven stopped state")
		}
		if err := m.RestoreBackup(s, b.ID, "test"); err == nil {
			t.Fatal("restore accepted unproven stopped state")
		}
		got, err := os.ReadFile(filepath.Join(m.serverDir(s), "world/level.dat"))
		if err != nil || string(got) != "original" {
			t.Fatal("refused restore changed world")
		}
	}
}

func TestFinalCapacityLeaseCoordinatesFilesystemAndCancellation(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	os.Mkdir(a, 0700)
	os.Mkdir(b, 0700)
	release, err := acquireCapacity(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := acquireCapacity(ctx, b); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lease = %v", err)
	}
}

func TestFinalTaskWaitCancellationJoinsWorkers(t *testing.T) {
	_, m := newTestAgent(t)
	s := m.List()[0]
	task, err := m.sched.Add(&Task{ServerID: s.ID, Name: "cancel", Time: "23:59", Commands: "!wait 900", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.sched.Run(s.ID, task.ID, "test") }()
	deadline := time.Now().Add(time.Second)
	for {
		current := m.sched.Get(task.ID)
		if current.RunState == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run not claimed")
		}
		time.Sleep(time.Millisecond)
	}
	m.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait result %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("task leaked")
	}
	if state := m.sched.Get(task.ID); state.RunState != "failed" || !strings.Contains(state.LastErr, "canceled") {
		t.Fatalf("cancellation not recorded: %+v", state)
	}
}

func TestFinalBindFailureStartsNoStorageOrWorkers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	path := filepath.Join(t.TempDir(), "must-not-create")
	if err := RunContext(context.Background(), Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, DataDir: path, NoAuth: true}); err == nil {
		t.Fatal("occupied listener accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("bind failure touched storage")
	}
}

func TestFinalMutationBoundaryAndStrictJSON(t *testing.T) {
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		if err := decodeJSON(r, &body); err != nil {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(204)
	})
	h := mutationPolicy("https://panel.example.com", downstream)
	for _, tc := range []struct {
		body, media, origin string
		code                int
	}{
		{`{"Name":"x"}`, "application/json", "", 204},
		{`{"Name":"x","typo":true}`, "application/json", "", 400},
		{`{"Name":"x"}{}`, "application/json", "", 400},
		{`{"Name":"x"}`, "text/plain", "", 415},
		{`{"Name":"x"}`, "application/json", "https://evil.example.com", 403},
	} {
		req := httptest.NewRequest("POST", "https://panel.example.com/api", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.media)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Fatalf("%+v code=%d", tc, w.Code)
		}
	}
}

func TestFinalRestoreJournalRemovesNewNamespaceOnRecovery(t *testing.T) {
	_, m := newTestAgent(t)
	defer m.Close()
	s := m.List()[0]
	dir := m.serverDir(s)
	staging := filepath.Join(dir, ".arcade-restore-final-fixture")
	os.MkdirAll(filepath.Join(staging, "old"), 0700)
	os.WriteFile(filepath.Join(staging, "old", "original"), []byte("old"), 0600)
	os.WriteFile(filepath.Join(dir, "new-only"), []byte("new"), 0600)
	if err := writeRestoreJournal(staging, restoreJournal{Phase: "installing", New: []string{"new-only"}, OldPort: s.Port, OldProps: s.Props}); err != nil {
		t.Fatal(err)
	}
	m.recoverInterruptedRestores()
	if _, err := os.Stat(filepath.Join(dir, "new-only")); !os.IsNotExist(err) {
		t.Fatal("new namespace leaked")
	}
	if got, err := os.ReadFile(filepath.Join(dir, "original")); err != nil || string(got) != "old" {
		t.Fatal("original not restored")
	}
}

func TestFinalTypedEngineInspectorFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   ContainerState
	}{
		{404, `{"message":"missing"}`, ContainerMissing},
		{200, `{"State":{"Running":true}}`, ContainerRunning},
		{200, `{"State":{"Running":false}}`, ContainerStopped},
		{200, `{}`, ContainerUnknown},
		{200, `{"State":{"Running":false}}{}`, ContainerUnknown},
		{401, `{}`, ContainerUnknown}, {403, `{}`, ContainerUnknown}, {503, `{}`, ContainerUnknown},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
		got := inspectEngineContainer(context.Background(), server.Client(), server.URL, "fixture")
		server.Close()
		if got != tc.want {
			t.Fatalf("%+v => %v", tc, got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := inspectEngineContainer(ctx, http.DefaultClient, "http://127.0.0.1:1", "fixture"); got != ContainerUnknown {
		t.Fatal("canceled probe authorized destructive action")
	}
}

func TestFinalAtomicPublicationFailureDistinguishesPublished(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := syncStateDirectory
	defer func() { syncStateDirectory = old; storageUncertain.Store(false) }()
	syncStateDirectory = func(string) error { return errors.New("sync failure") }
	result := atomicStateWrite(path, []byte("candidate"), 0600)
	if !result.Published || result.Err == nil {
		t.Fatalf("publication result=%+v", result)
	}
	if got, _ := os.ReadFile(path); string(got) != "candidate" {
		t.Fatal("candidate not published")
	}
	if err := writeFileAtomic(path, []byte("next"), 0600); err != nil || !storageUncertain.Load() {
		t.Fatal("published state was falsely rejected or health not marked")
	}
}
