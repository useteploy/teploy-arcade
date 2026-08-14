package arcade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- Phase 5

// The whole security model of the file manager is one function. If it is wrong,
// every other file check is decoration.
func TestFilePathsCannotEscapeServerDir(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	if _, err := mgr.ensureServerDir(s); err != nil {
		t.Fatal(err)
	}

	escapes := []string{
		"../servers.json",
		"../../etc/passwd",
		"world/../../../etc/passwd",
		"/etc/passwd",
		"world/../..",
		`..\..\servers.json`,
	}
	for _, p := range escapes {
		if _, err := mgr.ReadFile(s, p); err == nil {
			t.Errorf("ReadFile(%q) was allowed; it must be refused", p)
		}
		if err := mgr.WriteFile(s, p, "pwned"); err == nil {
			t.Errorf("WriteFile(%q) was allowed; it must be refused", p)
		}
	}

	// The panel's own state file must still be there and unmodified.
	if b, err := os.ReadFile(mgr.storePath()); err != nil || strings.Contains(string(b), "pwned") {
		t.Fatal("servers.json was reachable from the file API")
	}

	// A symlink planted inside the tree must not become an escape hatch.
	dir := mgr.serverDir(s)
	link := filepath.Join(dir, "escape")
	if err := os.Symlink(filepath.Dir(mgr.dataDir), link); err == nil {
		if _, err := mgr.ListFiles(s, "escape"); err == nil {
			t.Error("a symlink out of the server directory was followed")
		}
	}
}

func TestFileReadWriteRoundTrip(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	entries, err := mgr.ListFiles(s, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var sawProps bool
	for _, e := range entries {
		if e.Name == "server.properties" {
			sawProps = true
			if !e.Text {
				t.Error("server.properties should be editable in the browser")
			}
		}
	}
	if !sawProps {
		t.Fatal("a new server has no server.properties")
	}

	if err := mgr.WriteFile(s, "plugins/config.yml", "debug: true\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := mgr.ReadFile(s, "plugins/config.yml")
	if err != nil || got != "debug: true\n" {
		t.Fatalf("round trip failed: %q %v", got, err)
	}
}

// Editing server.properties by hand must move the panel's model too, or the
// settings screen and the file silently disagree.
func TestEditingPropertiesFileUpdatesSettings(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	content, err := mgr.ReadFile(s, "server.properties")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content = strings.Replace(content, "difficulty=normal", "difficulty=hard", 1)
	content = strings.Replace(content, "max-players=20", "max-players=64", 1)

	if err := mgr.WriteFile(s, "server.properties", content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if s.Props["difficulty"] != "hard" {
		t.Errorf("difficulty = %q, want hard", s.Props["difficulty"])
	}
	if s.MaxPlayers != 64 {
		t.Errorf("MaxPlayers = %d, want 64", s.MaxPlayers)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	if err := mgr.WriteFile(s, "world/important.txt", "original world"); err != nil {
		t.Fatal(err)
	}
	b, err := mgr.CreateBackup(s, "before the experiment", "tester")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if b.Size == 0 {
		t.Error("backup archive is empty")
	}

	// Wreck it.
	if err := mgr.WriteFile(s, "world/important.txt", "ruined"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.DeletePath(s, "eula.txt"); err != nil {
		t.Fatal(err)
	}

	if err := mgr.RestoreBackup(s, b.ID, "tester"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	got, err := mgr.ReadFile(s, "world/important.txt")
	if err != nil || got != "original world" {
		t.Fatalf("restore did not bring the world back: %q %v", got, err)
	}
	if _, err := mgr.ReadFile(s, "eula.txt"); err != nil {
		t.Error("a file deleted after the backup was not restored")
	}

	list, err := mgr.ListBackups(s)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListBackups = %d entries, %v", len(list), err)
	}
	if list[0].Note != "before the experiment" {
		t.Errorf("note lost: %q", list[0].Note)
	}
}

// Restoring under a running server would have the game writing into a tree
// being replaced beneath it.
func TestRestoreRefusedWhileRunning(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := startServer(t, mgr)

	b, err := mgr.CreateBackup(s, "", "tester")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := mgr.RestoreBackup(s, b.ID, "tester"); err == nil {
		t.Fatal("restore was allowed while the server was running")
	}
}

// The quiesce window must block writes, so a snapshot can never catch a
// half-written file (PLAN.md §8).
func TestBackupBlocksWritesWhileRunning(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	if !mgr.lockBackup(s.ID) {
		t.Fatal("could not take the backup lock")
	}
	err := mgr.WriteFile(s, "world/level.dat", "should not land")
	mgr.unlockBackup(s.ID)

	if err == nil {
		t.Fatal("a write was accepted during the quiesce window")
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Errorf("error should explain why: %v", err)
	}
}

// ---------------------------------------------------------------- Phase 4

func TestMetricsSeriesIsBoundedAndThinned(t *testing.T) {
	m := NewMetrics()
	now := time.Now().Unix()
	for i := 0; i < samplesKept+250; i++ {
		m.push("srv", Sample{T: now - int64(samplesKept+250-i), CPU: float64(i % 100)})
	}
	all := m.Series("srv", 0, 0)
	if len(all) != samplesKept {
		t.Errorf("ring kept %d samples, want %d", len(all), samplesKept)
	}
	thinned := m.Series("srv", 0, 60)
	if len(thinned) != 60 {
		t.Errorf("thinned to %d points, want 60", len(thinned))
	}
	// the newest sample must survive thinning, or the graph lies about "now"
	if thinned[len(thinned)-1] != all[len(all)-1] {
		t.Error("thinning dropped the most recent sample")
	}
}

// ---------------------------------------------------------------- Phase 8

func TestRolesAreEnforced(t *testing.T) {
	_, mgr := newTestAgent(t)
	a := mgr.auth

	if a.Enabled() {
		t.Fatal("auth should start disabled on a fresh data dir")
	}
	if _, err := a.CreateUser("admin", "short", RoleAdmin); err == nil {
		t.Error("a short password was accepted")
	}
	if _, err := a.CreateUser("admin", "correct-horse", RoleAdmin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if !a.Enabled() {
		t.Fatal("auth should switch on once a user exists")
	}
	if _, err := a.CreateUser("admin", "another-one", RoleAdmin); err == nil {
		t.Error("duplicate user was accepted")
	}

	if _, err := a.Login("admin", "wrong"); err == nil {
		t.Fatal("login succeeded with the wrong password")
	}
	sess, err := a.Login("admin", "correct-horse")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if a.Session(sess.Token) == nil {
		t.Fatal("session not resolvable straight after login")
	}

	if _, err := a.CreateUser("watcher", "watch-me-now", RoleViewer); err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	if roleRank[RoleViewer] >= roleRank[RoleOperator] {
		t.Error("viewer must rank below operator")
	}

	// The last admin must not be removable, or the panel locks everyone out.
	if err := a.DeleteUser("admin"); err == nil {
		t.Error("the last admin was deleted")
	}
	if err := a.DeleteUser("watcher"); err != nil {
		t.Errorf("deleting a viewer failed: %v", err)
	}

	a.Logout(sess.Token)
	if a.Session(sess.Token) != nil {
		t.Error("session survived logout")
	}
}

func TestPasswordHashingIsSaltedAndStable(t *testing.T) {
	h1 := hashPassword("same-password", "salt-a")
	h2 := hashPassword("same-password", "salt-b")
	h3 := hashPassword("same-password", "salt-a")

	if h1 == h2 {
		t.Error("different salts produced the same hash")
	}
	if h1 != h3 {
		t.Error("hashing is not deterministic")
	}
	if len(h1) != 64 {
		t.Errorf("hash is %d hex chars, want 64", len(h1))
	}
	if strings.Contains(h1, "same-password") {
		t.Error("the password leaked into its own hash")
	}
}

func TestAuditIsAppendOnlyAndNewestFirst(t *testing.T) {
	_, mgr := newTestAgent(t)
	mgr.audit("tester", "server.start", "s1", "")
	mgr.audit("tester", "server.stop", "s1", "")
	mgr.audit("other", "user.create", "bob", "viewer")

	got := mgr.auth.Audit(10)
	if len(got) != 3 {
		t.Fatalf("audit has %d entries, want 3", len(got))
	}
	if got[0].Action != "user.create" {
		t.Errorf("newest entry = %q, want user.create", got[0].Action)
	}
	if got[0].TS < got[2].TS {
		t.Error("entries are not newest-first")
	}
	if n := len(mgr.auth.Audit(2)); n != 2 {
		t.Errorf("limit ignored: got %d", n)
	}
}

// ---------------------------------------------------------------- Phase 6

func TestTemplatesLoadFromDiskAndValidate(t *testing.T) {
	dir := t.TempDir()

	// First load seeds the directory from the built-ins.
	if err := LoadTemplates(dir); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seeded := allTemplates()
	if len(seeded) < 5 {
		t.Fatalf("only %d templates after seeding", len(seeded))
	}

	// A new game is a file drop, no code change - that is Phase 6's DoD.
	custom := `{
	  "slug": "terraria", "name": "Terraria", "game": "terraria",
	  "group": "Other", "mark": "valheim",
	  "description": "Dedicated Terraria server.",
	  "maturity": "preview", "image": "ryshe/terraria",
	  "versions": ["1.4.4.9"], "memory_mb": 2048, "cpu": 1,
	  "disk_gb": 5, "max_players": 8, "port_hint": 7777
	}`
	if err := os.WriteFile(filepath.Join(templatesDir(dir), "terraria.json"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	// A malformed one must be skipped, not fatal.
	if err := os.WriteFile(filepath.Join(templatesDir(dir), "broken.json"), []byte(`{"slug":"broken"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	err := LoadTemplates(dir)
	if err == nil || !strings.Contains(err.Error(), "skipped") {
		t.Errorf("a malformed template should be reported, got %v", err)
	}

	if tpl := templateBySlugLoaded("terraria"); tpl == nil {
		t.Fatal("the dropped-in template was not picked up")
	} else if tpl.MaxPlayers != 8 {
		t.Errorf("MaxPlayers = %d, want 8", tpl.MaxPlayers)
	}
	if templateBySlugLoaded("broken") != nil {
		t.Error("an invalid template was loaded anyway")
	}

	// Restore the built-in set for the other tests in this package.
	tplMu.Lock()
	tplLoaded = nil
	tplMu.Unlock()
}

// The JVM heap must sit below the container limit, or the kernel kills the
// server as the heap grows. Regression test for a real failure: a 2 GB server
// with a 2 GB heap died during chunk generation.
func TestJVMHeapLeavesHeadroomUnderTheContainerLimit(t *testing.T) {
	for _, limit := range []int{1024, 2048, 4096, 6144, 8192, 16384} {
		heap := jvmHeapMB(limit)
		if heap >= limit {
			t.Errorf("limit %d MB -> heap %d MB: heap must be below the limit", limit, heap)
		}
		if limit-heap < 512 {
			t.Errorf("limit %d MB -> heap %d MB: only %d MB reserved, want >= 512",
				limit, heap, limit-heap)
		}
		if heap < 512 {
			t.Errorf("limit %d MB -> heap %d MB: too small for Minecraft to start", limit, heap)
		}
	}
}

// ---------------------------------------------------- players + scheduler

// While the server is running the game owns its own files, so a change must go
// through the console. Writing the file underneath a running server means the
// game overwrites it on next save and the change silently vanishes.
func TestPlayerListsRouteThroughTheGameWhenRunning(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	// stopped -> written straight to disk
	if err := mgr.AddToList(s, ListWhitelist, "Notch", "", "tester"); err != nil {
		t.Fatalf("add: %v", err)
	}
	entries, err := mgr.readList(s, ListWhitelist)
	if err != nil || len(entries) != 1 || entries[0].Name != "Notch" {
		t.Fatalf("whitelist = %v (%v)", entries, err)
	}
	if err := mgr.AddToList(s, ListWhitelist, "Notch", "", "tester"); err == nil {
		t.Error("adding a duplicate should be refused")
	}
	if err := mgr.RemoveFromList(s, ListWhitelist, "notch", "tester"); err != nil {
		t.Errorf("remove should be case-insensitive: %v", err)
	}

	// running -> the change must travel through the game console, not a direct
	// file write. Measured by the game acknowledging the command: a direct
	// write would change the file with nothing appearing in the stream.
	startServer(t, mgr)
	if err := mgr.AddToList(s, ListOps, "Herobrine", "", "tester"); err != nil {
		t.Fatalf("add while running: %v", err)
	}

	sawAck := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !sawAck {
		for _, l := range mgr.hub.Tail(s.ID, 40) {
			if strings.Contains(l.Text, "Herobrine a server operator") {
				sawAck = true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !sawAck {
		t.Error("no console acknowledgement: the change did not go through the game")
	}

	// and the list itself reflects it, the way a real server's file would
	ops, _ := mgr.readList(s, ListOps)
	found := false
	for _, e := range ops {
		if e.Name == "Herobrine" {
			found = true
		}
	}
	if !found {
		t.Error("the operator list does not reflect the command the game acknowledged")
	}
}

func TestPlayerListValidation(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	if err := mgr.AddToList(s, ListWhitelist, "  ", "", "t"); err == nil {
		t.Error("blank name accepted")
	}
	if err := mgr.AddToList(s, ListWhitelist, "two words", "", "t"); err == nil {
		t.Error("name with a space accepted")
	}
	if err := mgr.AddToList(s, "nonsense", "x", "", "t"); err == nil {
		t.Error("unknown list accepted")
	}
	if err := mgr.RemoveFromList(s, ListBanned, "ghost", "t"); err == nil {
		t.Error("removing an absent entry should say so")
	}
	// bans carry a default reason so the game file stays valid
	if err := mgr.AddToList(s, ListBanned, "Griefer", "", "t"); err != nil {
		t.Fatal(err)
	}
	e, _ := mgr.readList(s, ListBanned)
	if len(e) != 1 || e[0].Reason == "" {
		t.Errorf("ban entry missing a reason: %+v", e)
	}
}

func TestSchedulerClockParsingAndNextRun(t *testing.T) {
	for _, bad := range []string{"", "25:00", "12:60", "12", "12:00:61", "noon"} {
		if _, err := parseClock(bad); err == nil {
			t.Errorf("parseClock(%q) should fail", bad)
		}
	}
	if got, err := parseClock("04:30"); err != nil || got != 4*3600+30*60 {
		t.Errorf("parseClock(04:30) = %d, %v", got, err)
	}

	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.Local)

	// a daily task whose time has passed rolls to tomorrow
	daily := &Task{Time: "04:00:00", Repeat: true, Enabled: true}
	next := daily.NextRun(now)
	if next.Day() != 13 || next.Hour() != 4 {
		t.Errorf("daily next run = %v, want tomorrow 04:00", next)
	}
	// a daily task later today stays today
	later := &Task{Time: "23:00:00", Repeat: true, Enabled: true}
	if n := later.NextRun(now); n.Day() != 12 || n.Hour() != 23 {
		t.Errorf("later-today next run = %v", n)
	}
	// a one-shot whose moment has passed has no next run
	once := &Task{Time: "04:00:00", Repeat: false, Enabled: true}
	if n := once.NextRun(now); !n.IsZero() {
		t.Errorf("expired one-shot should have no next run, got %v", n)
	}
}

func TestSchedulerRunsStepsAndPanelActions(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := startServer(t, mgr)

	if _, err := mgr.sched.Add(&Task{ServerID: s.ID, Name: "bad time", Commands: "say hi", Time: "99:99"}); err == nil {
		t.Error("invalid time accepted")
	}
	if _, err := mgr.sched.Add(&Task{ServerID: s.ID, Name: "", Commands: "say hi", Time: "04:00"}); err == nil {
		t.Error("nameless task accepted")
	}
	if _, err := mgr.sched.Add(&Task{ServerID: "nope", Name: "x", Commands: "say hi", Time: "04:00"}); err == nil {
		t.Error("task for an unknown server accepted")
	}

	// console command + panel action in one task
	task, err := mgr.sched.Add(&Task{
		ServerID: s.ID, Name: "nightly", Commands: "say backing up; !backup nightly",
		Time: "04:00:00", Repeat: true,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := mgr.sched.Run(task.ID, "tester"); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := mgr.sched.Get(task.ID)
	if got.Runs != 1 || got.LastRun == 0 || got.LastErr != "" {
		t.Fatalf("after run: %+v", got)
	}
	backups, _ := mgr.ListBackups(s)
	if len(backups) != 1 {
		t.Fatalf("!backup did not produce an archive: %d", len(backups))
	}

	// an unknown action must report itself rather than fail silently
	bad, _ := mgr.sched.Add(&Task{ServerID: s.ID, Name: "bad", Commands: "!explode", Time: "05:00", Repeat: true})
	if err := mgr.sched.Run(bad.ID, "tester"); err == nil {
		t.Error("unknown action should error")
	}
	if e := mgr.sched.Get(bad.ID); e.LastErr == "" {
		t.Error("failure not recorded on the task")
	}

	// one-shot disables itself after firing
	once, _ := mgr.sched.Add(&Task{ServerID: s.ID, Name: "once", Commands: "say bye", Time: "06:00", Repeat: false})
	_ = mgr.sched.Run(once.ID, "tester")
	if mgr.sched.Get(once.ID).Enabled {
		t.Error("a one-shot task should disable itself after running")
	}
}
