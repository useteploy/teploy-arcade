package arcade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFinalPlayerIdentityOfflineUsesJavaUUIDAndNameCase(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	s.mu.Lock()
	s.Props["online-mode"] = "false"
	s.mu.Unlock()
	if got := offlinePlayerUUID("Notch"); got != "b50ad385-829d-3141-a216-7e7d7539ba7f" {
		t.Fatalf("Java offline UUID mismatch: %s", got)
	}
	if offlinePlayerUUID("notch") == offlinePlayerUUID("Notch") {
		t.Fatal("offline name case was discarded")
	}
	if err := mgr.AddToList(s, ListWhitelist, "Notch", "", "test"); err != nil {
		t.Fatal(err)
	}
	list, err := mgr.readList(s, ListWhitelist)
	if err != nil || len(list) != 1 || list[0].UUID != "b50ad385-829d-3141-a216-7e7d7539ba7f" {
		t.Fatalf("offline list identity: %v %v", list, err)
	}
}

func TestFinalPlayerIdentityUnknownOnlineRefusesWithoutWrite(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	path := filepath.Join(mgr.serverDir(s), "whitelist.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.AddToList(s, ListWhitelist, "UnknownUser", "", "test"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown online identity admitted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("refused identity changed list bytes")
	}
}

func TestFinalPlayerIdentityOnlineCacheAndExplicitKnownList(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	known := "069a79f4-44e9-4726-a5be-fca90e38aaf5"
	cache := `[{"name":"Notch","uuid":"069A79F4-44E9-4726-A5BE-FCA90E38AAF5","expiresOn":"future metadata"}]`
	if err := os.WriteFile(filepath.Join(mgr.serverDir(s), "usercache.json"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AddToList(s, ListOps, "notch", "", "test"); err != nil {
		t.Fatal(err)
	}
	list, err := mgr.readList(s, ListOps)
	if err != nil || len(list) != 1 || list[0].UUID != known {
		t.Fatalf("cached UUID not used: %v %v", list, err)
	}
	if err := os.Remove(filepath.Join(mgr.serverDir(s), "usercache.json")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AddToList(s, ListWhitelist, "Notch", "", "test"); err != nil {
		t.Fatalf("explicit matching list identity not reused: %v", err)
	}
	list, err = mgr.readList(s, ListWhitelist)
	if err != nil || len(list) != 1 || list[0].UUID != known {
		t.Fatal("known list UUID not retained")
	}
}

func TestFinalPlayerIdentityAmbiguousMalformedOrOversizeCacheRefuses(t *testing.T) {
	for name, cache := range map[string]string{
		"ambiguous":    `[{"name":"Notch","uuid":"069a79f4-44e9-4726-a5be-fca90e38aaf5"},{"name":"notch","uuid":"b50ad385-829d-3141-a216-7e7d7539ba7f"}]`,
		"invalid uuid": `[{"name":"Notch","uuid":"placeholder"}]`, "nil uuid": `[{"name":"Notch","uuid":"00000000-0000-0000-0000-000000000000"}]`, "null profile": `[null]`, "malformed": `{`, "oversize": strings.Repeat(" ", maxIdentityCacheBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, mgr := newTestAgent(t)
			s := mgr.List()[0]
			if err := os.WriteFile(filepath.Join(mgr.serverDir(s), "usercache.json"), []byte(cache), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(mgr.serverDir(s), "ops.json")
			before, _ := os.ReadFile(path)
			if err := mgr.AddToList(s, ListOps, "Notch", "", "test"); err == nil {
				t.Fatal("invalid identity evidence accepted")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("refused identity changed bytes")
			}
		})
	}
}

func TestFinalPlayerIdentityRootedCacheCannotEscape(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	outside := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(outside, []byte(`[{"name":"Notch","uuid":"069a79f4-44e9-4726-a5be-fca90e38aaf5"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mgr.serverDir(s), "usercache.json")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AddToList(s, ListWhitelist, "Notch", "", "test"); err == nil {
		t.Fatal("cache followed a symlink outside server root")
	}
}

func TestFinalPlayerIdentityConcurrentChangesPreserveRawFields(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	s.mu.Lock()
	s.Props["online-mode"] = "false"
	s.mu.Unlock()
	source := `[{"name":"Existing","uuid":"069a79f4-44e9-4726-a5be-fca90e38aaf5","level":2,"bypassesPlayerLimit":true,"plugin":{"role":"operator"}}]`
	path := filepath.Join(mgr.serverDir(s), "ops.json")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"Alice", "Bob"} {
		wg.Add(1)
		go func(name string) { defer wg.Done(); results <- mgr.AddToList(s, ListOps, name, "", "test") }(name)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	var plugin map[string]string
	if len(entries) > 0 {
		_ = json.Unmarshal(entries[0]["plugin"], &plugin)
	}
	if len(entries) != 3 || string(entries[0]["bypassesPlayerLimit"]) != "true" || plugin["role"] != "operator" {
		t.Fatalf("raw fields or concurrent add lost: %s", body)
	}
}

func TestFinalPlayerIdentityIPValidationAndEquivalentRemoval(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	if err := mgr.AddToList(s, ListBannedIPs, "not-an-ip", "", "test"); err == nil {
		t.Fatal("invalid IP accepted")
	}
	if err := mgr.AddToList(s, ListBannedIPs, "2001:0db8:0:0:0:0:0:1", "", "test"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AddToList(s, ListBannedIPs, "2001:db8::1", "", "test"); err == nil {
		t.Fatal("equivalent IP duplicate accepted")
	}
	if err := mgr.RemoveFromList(s, ListBannedIPs, "2001:0db8::1", "test"); err != nil {
		t.Fatalf("equivalent IP removal failed: %v", err)
	}
}

type finalPlayerSendRunner struct {
	sent    chan string
	release <-chan struct{}
}

func (r *finalPlayerSendRunner) Start(*Server, func(Line)) error { return nil }
func (r *finalPlayerSendRunner) Stop(*Server) error              { return nil }
func (r *finalPlayerSendRunner) Kill(*Server) error              { return nil }
func (r *finalPlayerSendRunner) Send(_ *Server, command string) error {
	r.sent <- command
	if r.release != nil {
		<-r.release
	}
	return nil
}

func TestFinalPlayerIdentitySnapshotGateBlocksLiveListCommands(t *testing.T) {
	for _, add := range []bool{true, false} {
		name := "remove"
		if add {
			name = "add"
		}
		t.Run(name, func(t *testing.T) {
			srv, mgr := newTestAgent(t)
			defer srv.Close()
			s := mgr.List()[0]
			s.mu.Lock()
			s.Status = StatusRunning
			s.mu.Unlock()
			runner := &finalPlayerSendRunner{sent: make(chan string, 1)}
			mgr.sim = runner
			s.fsMu.Lock()
			locked := true
			defer func() {
				if locked {
					s.fsMu.Unlock()
				}
			}()
			outcome := make(chan error, 1)
			go func() {
				if add {
					outcome <- mgr.AddToList(s, ListWhitelist, "KnownName", "", "test")
				} else {
					outcome <- mgr.RemoveFromList(s, ListWhitelist, "KnownName", "test")
				}
			}()
			select {
			case text := <-runner.sent:
				t.Fatalf("snapshot window received command bytes: %s", text)
			case err := <-outcome:
				t.Fatalf("snapshot gate bypassed by list change: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			s.fsMu.Unlock()
			locked = false
			select {
			case command := <-runner.sent:
				expected := gameCommand(ListWhitelist, add, "KnownName", "")
				if command != expected {
					t.Fatalf("command mismatch: %s", command)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("list command did not resume after snapshot gate")
			}
			select {
			case err := <-outcome:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("list change never completed")
			}
		})
	}
}

func TestFinalPlayerIdentitySnapshotGateCoversEntireLiveDelivery(t *testing.T) {
	srv, mgr := newTestAgent(t)
	defer srv.Close()
	s := mgr.List()[0]
	s.mu.Lock()
	s.Status = StatusRunning
	s.mu.Unlock()
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	runner := &finalPlayerSendRunner{sent: make(chan string, 1), release: release}
	mgr.sim = runner
	outcome := make(chan error, 1)
	go func() { outcome <- mgr.AddToList(s, ListOps, "KnownName", "", "test") }()
	select {
	case <-runner.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("command did not reach runner")
	}
	exclusive := make(chan struct{})
	go func() { s.fsMu.Lock(); close(exclusive); s.fsMu.Unlock() }()
	select {
	case <-exclusive:
		t.Fatal("snapshot acquired filesystem while command delivery was in progress")
	case <-time.After(50 * time.Millisecond):
	}
	finish()
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delivery never completed")
	}
	select {
	case <-exclusive:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot gate was not released after delivery")
	}
}
