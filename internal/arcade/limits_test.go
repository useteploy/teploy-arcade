package arcade

import (
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
