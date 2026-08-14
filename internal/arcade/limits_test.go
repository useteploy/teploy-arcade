package arcade

import (
	"fmt"
	"strings"
	"testing"
)

// Port, memory and CPU go straight onto a docker command line and into
// server.properties. Out-of-range values used to be accepted here and fail
// later at container start, with an error that points at docker rather than at
// the number someone typed.
func TestCheckServerLimits(t *testing.T) {
	cases := []struct {
		name    string
		port    int
		mem     int
		cpu     float64
		wantErr string
	}{
		{name: "all zero means use the template", port: 0, mem: 0, cpu: 0},
		{name: "ordinary values", port: 25565, mem: 2048, cpu: 2},
		{name: "boundaries are inclusive", port: 65535, mem: minServerMemMB, cpu: minServerCPU},
		{name: "port 1", port: 1, mem: 0, cpu: 0},

		{name: "port above range", port: 65536, wantErr: "port"},
		{name: "port negative", port: -1, wantErr: "port"},
		{name: "port far out", port: 70000, wantErr: "port"},

		{name: "memory below the JVM floor", mem: 511, wantErr: "memory"},
		{name: "memory of 1 MB", mem: 1, wantErr: "memory"},
		{name: "memory negative", mem: -2048, wantErr: "memory"},
		{name: "memory absurd", mem: maxServerMemMB + 1, wantErr: "memory"},

		{name: "cpu too small for docker", cpu: 0.001, wantErr: "cpu"},
		{name: "cpu negative", cpu: -1, wantErr: "cpu"},
		{name: "cpu absurd", cpu: maxServerCPU + 1, wantErr: "cpu"},
	}

	for _, c := range cases {
		err := checkServerLimits(c.port, c.mem, c.cpu)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: rejected a valid combination: %v", c.name, err)
		case c.wantErr != "" && err == nil:
			t.Errorf("%s: accepted port=%d mem=%d cpu=%v", c.name, c.port, c.mem, c.cpu)
		case c.wantErr != "" && err != nil && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("%s: error should name the field at fault, got %v", c.name, err)
		}
	}
}

// The bound has to hold at the boundary every caller crosses, not in one HTTP
// handler. Create is one of the two ways a server comes into existence.
func TestCreateRefusesOutOfRangeResources(t *testing.T) {
	_, mgr := newTestAgent(t)
	before := len(mgr.List())

	bad := []struct {
		name string
		port int
		mem  int
		cpu  float64
	}{
		{"port", 70000, 0, 0},
		{"memory", 0, 64, 0},
		{"cpu", 0, 0, 0.0001},
	}
	for _, b := range bad {
		if _, err := mgr.Create("bad "+b.name, "paper", "1.20.4", b.port, b.mem, b.cpu, RuntimeSim); err == nil {
			t.Errorf("Create accepted an out-of-range %s", b.name)
		}
	}
	if got := len(mgr.List()); got != before {
		t.Errorf("a refused Create still registered a server: %d -> %d", before, got)
	}

	// And a legitimate one still works, so the guard is not just an outage.
	s, err := mgr.Create("fine", "paper", "1.20.4", 0, 2048, 2, RuntimeSim)
	if err != nil {
		t.Fatalf("Create refused a valid server: %v", err)
	}
	if s.MemoryMB != 2048 || s.CPU != 2 {
		t.Errorf("resources not applied: mem=%d cpu=%v", s.MemoryMB, s.CPU)
	}
}

// Import is the other way, and it reaches those fields by a different route -
// the site a fix aimed at Create alone would miss. It is checked in
// StartImport rather than at the end of the copy, so an operator learns before
// waiting for gigabytes to move.
func TestImportRefusesOutOfRangeResources(t *testing.T) {
	_, mgr := newImportAgent(t)
	dir := mkTree(t, "importable", map[string]string{
		"paper-1.20.4.jar":  "PK\x03\x04 jar",
		"server.properties": "server-port=25599\nmotd=Imported\n",
		"world/level.dat":   "level",
	})

	for _, b := range []struct {
		name string
		req  ImportRequest
	}{
		{"memory", ImportRequest{Path: dir, Name: "im", Mode: ImportCopy, MemoryMB: 64}},
		{"cpu", ImportRequest{Path: dir, Name: "im", Mode: ImportCopy, CPU: 0.0001}},
		{"port", ImportRequest{Path: dir, Name: "im", Mode: ImportCopy, Port: 70000}},
	} {
		job, err := mgr.StartImport(b.req, "tester")
		if err == nil {
			t.Errorf("StartImport accepted an out-of-range %s (job %v)", b.name, job)
		}
	}
}

// Resources were settable only at Create/Import: PATCH /api/servers/{id} was a
// 404, so giving an imported modpack more memory meant stopping the panel and
// hand-editing servers.json. Found migrating RL Craft, which needs 6 GB.
func TestServerResourcesCanBeChangedAfterCreation(t *testing.T) {
	_, mgr := newTestAgent(t)
	s, err := mgr.Create("resizable", "paper", "1.20.4", 0, 2048, 2, RuntimeSim)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := mgr.SetResources(s, 6144, 4); err != nil {
		t.Fatalf("set resources: %v", err)
	}
	if s.MemoryMB != 6144 || s.CPU != 4 {
		t.Errorf("resources not applied: mem=%d cpu=%v", s.MemoryMB, s.CPU)
	}

	// 0 means "leave this one alone", as it does everywhere else.
	if _, err := mgr.SetResources(s, 0, 3); err != nil {
		t.Fatalf("cpu-only change: %v", err)
	}
	if s.MemoryMB != 6144 {
		t.Errorf("a cpu-only change altered memory: %d", s.MemoryMB)
	}
	if s.CPU != 3 {
		t.Errorf("cpu = %v, want 3", s.CPU)
	}

	// The same bounds as Create and StartImport, or the limit is only a limit
	// on the paths someone happened to think of.
	for _, bad := range []struct {
		mem int
		cpu float64
	}{{64, 0}, {0, 0.0001}, {maxServerMemMB + 1, 0}, {0, maxServerCPU + 1}} {
		if _, err := mgr.SetResources(s, bad.mem, bad.cpu); err == nil {
			t.Errorf("SetResources accepted out-of-range mem=%d cpu=%v", bad.mem, bad.cpu)
		}
	}
	if s.MemoryMB != 6144 || s.CPU != 3 {
		t.Errorf("a refused change still altered the server: mem=%d cpu=%v", s.MemoryMB, s.CPU)
	}

	// A no-op is refused rather than silently succeeding.
	if _, err := mgr.SetResources(s, 0, 0); err == nil {
		t.Error("SetResources accepted a request that changed nothing")
	}

	// The change reaches the container command, which is the whole point.
	args := dockerRunArgs(s, "gamepanel-x", "/srv/data", "sec")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--memory 6144m") {
		t.Errorf("new memory limit not in the container command:\n%s", joined)
	}
	if !strings.Contains(joined, "--cpus 3.00") {
		t.Errorf("new cpu limit not in the container command:\n%s", joined)
	}
}

// Order was creation order with no way to change it. That stops being a detail
// once there are more servers than fit on screen: the tab strip is how you move
// between them, and you could not put the one you watch all day first.
func TestServersCanBeReordered(t *testing.T) {
	_, mgr := newTestAgent(t)
	for _, n := range []string{"alpha", "bravo", "charlie"} {
		if _, err := mgr.Create(n, "paper", "1.20.4", 0, 2048, 2, RuntimeSim); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	names := func() []string {
		out := []string{}
		for _, s := range mgr.List() {
			out = append(out, s.Name)
		}
		return out
	}
	byName := map[string]string{}
	for _, s := range mgr.List() {
		byName[s.Name] = s.ID
	}

	// Reverse the three we created, leaving the seeded ones alone.
	if err := mgr.Reorder([]string{byName["charlie"], byName["bravo"], byName["alpha"]}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	got := names()
	if got[0] != "charlie" || got[1] != "bravo" || got[2] != "alpha" {
		t.Errorf("order = %v, want charlie/bravo/alpha first", got[:3])
	}

	// Servers the caller did not mention keep their relative order and follow.
	// A client holding a stale list must not silently drop a server created
	// while the operator was mid-drag.
	before := names()
	if err := mgr.Reorder([]string{byName["alpha"]}); err != nil {
		t.Fatalf("partial reorder: %v", err)
	}
	after := names()
	if after[0] != "alpha" {
		t.Errorf("named server did not move to the front: %v", after[:2])
	}
	if len(after) != len(before) {
		t.Errorf("reorder changed the server count: %d -> %d", len(before), len(after))
	}

	// An id for a server that no longer exists is ignored, not fatal - it may
	// have been deleted while the drag was in flight.
	if err := mgr.Reorder([]string{"s-does-not-exist", byName["bravo"]}); err != nil {
		t.Fatalf("reorder with a stale id: %v", err)
	}
	if names()[0] != "bravo" {
		t.Errorf("a stale id stopped the rest of the reorder: %v", names()[:2])
	}
	if len(names()) != len(before) {
		t.Errorf("a stale id changed the server count")
	}

	// Duplicates cannot clone a server into the list.
	id := byName["alpha"]
	if err := mgr.Reorder([]string{id, id, id}); err != nil {
		t.Fatalf("reorder with duplicates: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range mgr.List() {
		if seen[s.ID] {
			t.Fatalf("server %s appears twice after a duplicate reorder", s.ID)
		}
		seen[s.ID] = true
	}
	if len(names()) != len(before) {
		t.Errorf("duplicates changed the server count: %v", names())
	}
}

// The order has to outlive the process, or it is a UI toy rather than a
// preference: the panel restarts on every upgrade.
func TestReorderSurvivesAPanelRestart(t *testing.T) {
	dir := t.TempDir()

	hub := NewHub()
	mgr := NewManager(dir, hub)
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, n := range []string{"one", "two", "three"} {
		if _, err := mgr.Create(n, "paper", "1.20.4", 0, 2048, 2, RuntimeSim); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	byName := map[string]string{}
	for _, s := range mgr.List() {
		byName[s.Name] = s.ID
	}
	want := []string{byName["three"], byName["one"], byName["two"]}
	if err := mgr.Reorder(want); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	// A second manager over the same directory is what a restart looks like.
	again := NewManager(dir, NewHub())
	if err := again.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := again.List()
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("position %d after restart is %q (%s), want %q",
				i, got[i].Name, got[i].ID, id)
		}
	}
}

// The settings screen shows the heap a server will actually get, so the number
// has to come from the same function the container is started with. It used to
// be recomputed in JavaScript, which is how the host tiles drifted.
func TestSnapshotReportsTheHeapTheContainerWillGet(t *testing.T) {
	_, mgr := newTestAgent(t)
	s, err := mgr.Create("sized", "paper", "1.20.4", 0, 4096, 2, RuntimeSim)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mem, ok := s.Snapshot()["memory"].(map[string]any)
	if !ok {
		t.Fatal("no memory block in the snapshot")
	}
	heap, ok := mem["heap_mb"].(int)
	if !ok {
		t.Fatalf("heap_mb missing or not an int: %#v", mem["heap_mb"])
	}
	if heap != jvmHeapMB(4096) {
		t.Errorf("snapshot heap %d, but the runner uses %d", heap, jvmHeapMB(4096))
	}

	// And it must match what the container is actually told.
	args := strings.Join(dockerRunArgs(s, "gamepanel-x", "/srv/data", "sec"), " ")
	if !strings.Contains(args, fmt.Sprintf("MEMORY=%dM", heap)) {
		t.Errorf("container is not given the heap the panel reports (%d MB):\n%s", heap, args)
	}

	// A change to the limit has to move the reported heap with it.
	if _, err := mgr.SetResources(s, 8192, 0); err != nil {
		t.Fatalf("resize: %v", err)
	}
	mem = s.Snapshot()["memory"].(map[string]any)
	if mem["heap_mb"].(int) != jvmHeapMB(8192) {
		t.Errorf("heap did not follow the new limit: %v", mem["heap_mb"])
	}
}
