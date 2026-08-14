package arcade

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writeFileAtomic used a temp file named path+".tmp", shared by every caller of
// the moment. Manager.Save is called from eight places across the HTTP handlers
// and the runner goroutines with no lock between them, so two saves would write
// the one temp name, the first rename would consume it and the second would
// fail ENOENT having written nothing. Six of those eight call sites are
// `_ = m.Save()`, so the state change vanished with no error anywhere.
//
// This surfaced as TestSettingsRestartFlagAndPortConflict failing intermittently
// once the lifecycle fixes changed how often the runner saves.
func TestConcurrentSavesDoNotDestroyEachOther(t *testing.T) {
	_, mgr := newTestAgent(t)

	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mgr.Save(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	failed := 0
	var first error
	for err := range errs {
		if first == nil {
			first = err
		}
		failed++
	}
	if failed > 0 {
		t.Fatalf("%d of %d concurrent saves failed, first: %v", failed, writers, first)
	}

	// The survivor must still be a whole file, not one writer's bytes laid over
	// another's.
	if err := NewManager(mgr.dataDir, NewHub()).Load(); err != nil {
		t.Fatalf("servers.json is unreadable after concurrent saves: %v", err)
	}
	// A temp file left in the data directory is loose state the next boot has to
	// step over; Load quarantines what it cannot parse, so litter here is not
	// harmless.
	ents, err := os.ReadDir(mgr.dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file %q was left behind", e.Name())
		}
	}
}

// The mode has to survive the rename. os.CreateTemp always makes 0600, so a
// helper that forgot to chmod would quietly tighten every 0644 state file and
// there is no second chance once the file is in place.
func TestAtomicWritesKeepTheRequestedMode(t *testing.T) {
	dir := t.TempDir()
	for _, perm := range []os.FileMode{0o600, 0o644} {
		p := filepath.Join(dir, "state-"+perm.String()+".json")
		if err := writeFileAtomic(p, []byte("{}\n"), perm); err != nil {
			t.Fatalf("write %v: %v", perm, err)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %v: %v", perm, err)
		}
		if st.Mode().Perm() != perm {
			t.Errorf("mode = %v, want %v", st.Mode().Perm(), perm)
		}
	}
}

// M11, the half that spanned two files. auth.go grew randomHex so the salt and
// the session token fail closed, but mcp.go still minted bearer tokens through
// randHex, which logs the entropy failure and returns "". Issue then stored
// hashPassword("tpa_") as a live token: the guessable prefix is a working
// credential for the whole MCP surface until someone revokes a token nobody
// knows is broken.
func TestMCPTokenIssueFailsClosedWithoutEntropy(t *testing.T) {
	toks := newMCPTokens(t.TempDir())

	orig := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("entropy pool is empty") }
	t.Cleanup(func() { randRead = orig })

	raw, err := toks.Issue("agent")
	if err == nil {
		t.Fatalf("Issue handed out %q with no entropy available", raw)
	}
	if raw != "" {
		t.Fatalf("Issue returned a token alongside its error: %q", raw)
	}
	if toks.Check("tpa_") {
		t.Fatal("the bare token prefix authenticates against the store")
	}
	if n := len(toks.toks); n != 0 {
		t.Fatalf("a token was persisted despite the failure: %d stored", n)
	}
}

// The seed write. seedServerFiles now reports a real failed create instead of
// swallowing a stat race, but seed() still discarded it, so the five first-run
// demo servers could be listed in the panel with no eula.txt, no ops.json and
// no server.properties. Nothing said so until someone pressed Start.
func TestSeedReportsFilesItCouldNotWrite(t *testing.T) {
	dir := t.TempDir()
	// A plain file where the servers/ tree belongs makes MkdirAll fail for
	// every seeded server - the shape a read-only or full disk takes.
	if err := os.WriteFile(filepath.Join(dir, "servers"), []byte("x"), 0o644); err != nil {
		t.Fatalf("plant blocker: %v", err)
	}

	sink := captureManagerLog(t)
	m := NewManager(dir, NewHub())
	m.seed()

	if !sink.contains("could not seed files") {
		t.Fatal("seed() swallowed every failed write; the panel lists five servers that cannot start")
	}
	if !sink.contains("My Purpur Server") {
		t.Fatal("the log line does not name the server that failed")
	}
}
