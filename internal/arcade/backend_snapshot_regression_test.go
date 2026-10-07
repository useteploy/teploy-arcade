package arcade

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func TestBackendSnapshotPendingRestartDetached(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"full capacity", []string{"Memory", "ViewDistance"}},
		{"spare capacity", append(make([]string, 0, 4), "Memory", "ViewDistance")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{PendingRestart: tc.pending}
			want := slices.Clone(tc.pending)
			got := s.Snapshot()["pending_restart"].([]string)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot changed nil/empty/value semantics: got %#v, want %#v", got, want)
			}
			if len(got) == 0 {
				return
			}
			got[0] = "snapshot-only"
			if !reflect.DeepEqual(s.PendingRestart, want) {
				t.Fatalf("mutating snapshot changed server: %v", s.PendingRestart)
			}
			fresh := s.Snapshot()["pending_restart"].([]string)
			s.mu.Lock()
			s.PendingRestart[0] = "server-only"
			s.mu.Unlock()
			if !reflect.DeepEqual(fresh, want) {
				t.Fatalf("mutating server changed an older snapshot: %v", fresh)
			}
		})
	}
}

func TestBackendSnapshotSurvivesSetResourcesSort(t *testing.T) {
	m := NewManager(t.TempDir(), NewHub())
	s := &Server{ID: "snapshot-proof", Name: "Snapshot proof", Runtime: RuntimeSim,
		Status: StatusRunning, MemoryMB: 2048, CPU: 1,
		Props: map[string]string{}, PendingRestart: make([]string, 2, 4)}
	copy(s.PendingRestart, []string{"Memory", "ViewDistance"})
	m.servers[s.ID] = s
	m.order = []string{s.ID}
	previousCPUs := hostCPUs
	hostCPUs = 2
	t.Cleanup(func() { hostCPUs = previousCPUs })

	got := s.Snapshot()["pending_restart"].([]string)
	want := slices.Clone(got)
	if _, err := m.SetResources(s, 0, 2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("old snapshot changed after resource append/sort: got %v, want %v", got, want)
	}
	if want := []string{"CPU", "Memory", "ViewDistance"}; !reflect.DeepEqual(s.PendingRestart, want) {
		t.Fatalf("resource update did not exercise the pending-restart sort: %v", s.PendingRestart)
	}
}

// Run with -race. The writer follows the same s.mu ownership as SetResources;
// marshaling a returned snapshot must never read the writer's backing array.
func TestBackendSnapshotConcurrentEncoding(t *testing.T) {
	s := &Server{PendingRestart: []string{"Memory", "ViewDistance"}}
	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for i := 0; i < 1000; i++ {
			s.mu.Lock()
			s.PendingRestart[0] = fmt.Sprintf("Memory-%d", i)
			s.mu.Unlock()
		}
	}()
	close(start)
	for i := 0; i < 1000; i++ {
		if _, err := json.Marshal(s.Snapshot()); err != nil {
			<-done
			t.Fatal(err)
		}
	}
	<-done
}
