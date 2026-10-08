package arcade

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFinalAuditStoreMigratesLegacyWithoutDestroyingSource(t *testing.T) {
	dir := t.TempDir()
	legacy := []AuditEntry{{TS: 1, Actor: "old", Action: "login", Detail: "rcon.password=private-value"}}
	body, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "audit.json"), body, 0644); err != nil {
		t.Fatal(err)
	}
	a := NewAuth(dir)
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	a.Append(AuditEntry{TS: 2, Actor: "new", Action: "stop"})
	b := NewAuth(dir)
	if err := b.Load(); err != nil {
		t.Fatal(err)
	}
	got := b.Audit(0)
	if len(got) != 2 || got[0].Actor != "new" || got[1].Actor != "old" {
		t.Fatalf("migration/reload lost records: %+v", got)
	}
	source, err := os.ReadFile(filepath.Join(dir, "audit.json"))
	if err != nil || string(source) != string(body) {
		t.Fatal("legacy evidence changed")
	}
	if strings.Contains(got[1].Detail, "private-value") {
		t.Fatal("migrated secret surfaced")
	}
	for _, name := range []string{a.auditSegmentPath(1), filepath.Join(a.auditDir(), ".migrated"), a.auditHealthPath(), filepath.Join(dir, "audit.json")} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private audit mode %s: %v %v", name, info, err)
		}
	}
}

func TestFinalAuditStoreRotationBoundAndOrderedReload(t *testing.T) {
	a := NewAuth(t.TempDir())
	a.auditSegmentLimit = 1024
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		a.Append(AuditEntry{TS: int64(i), Actor: "operator", Action: "test", Detail: strings.Repeat("d", 200)})
	}
	files, err := a.auditFilesLocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != auditSegmentsKept {
		t.Fatalf("rotation retained %d files", len(files))
	}
	total := int64(0)
	for _, name := range files {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	if total > 1024*auditSegmentsKept {
		t.Fatalf("retention exceeds byte bound: %d", total)
	}
	b := NewAuth(a.dataDir)
	b.auditSegmentLimit = 1024
	if err := b.Load(); err != nil {
		t.Fatal(err)
	}
	entries := b.Audit(0)
	if len(entries) == 0 || entries[0].TS != 49 {
		t.Fatal("newest durable record missing")
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].TS <= entries[i].TS {
			t.Fatal("durable ordering lost")
		}
	}
}

func TestFinalAuditStoreFailureQueuesThenRecoversDurableHealth(t *testing.T) {
	a := NewAuth(t.TempDir())
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(a.auditSegmentPath(1), 0700); err != nil {
		t.Fatal(err)
	}
	a.Append(AuditEntry{TS: 1, Action: "first"})
	if a.AuditHealth()["healthy"] != false || a.AuditHealth()["pending_records"] != 1 {
		t.Fatalf("failure not visible: %v", a.AuditHealth())
	}
	if err := os.Remove(a.auditSegmentPath(1)); err != nil {
		t.Fatal(err)
	}
	a.Append(AuditEntry{TS: 2, Action: "second"})
	b := NewAuth(a.dataDir)
	if err := b.Load(); err != nil {
		t.Fatal(err)
	}
	entries := b.Audit(0)
	if len(entries) != 2 || entries[0].TS != 2 || entries[1].TS != 1 {
		t.Fatalf("retry backlog lost/duplicated events: %v", entries)
	}
	if b.AuditHealth()["write_failures"] != uint64(1) {
		t.Fatalf("failure health not durable: %v", b.AuditHealth())
	}
}

func TestFinalAuditStoreSlowDiskDoesNotBlockSessions(t *testing.T) {
	a := NewAuth(t.TempDir())
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	a.sessions["live"] = &Session{Token: "live", User: "operator", Expires: time.Now().Add(time.Hour)}
	original := syncStateDirectory
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	syncStateDirectory = func(string) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}
	go func() { defer close(done); a.Append(AuditEntry{Action: "test"}) }()
	defer func() { close(release); <-done; syncStateDirectory = original }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("append did not reach disk barrier")
	}
	health := make(chan map[string]any, 1)
	go func() { health <- a.AuditHealth() }()
	select {
	case <-health:
	case <-time.After(time.Second):
		t.Fatal("audit disk I/O blocked health snapshot")
	}
	session := make(chan *Session, 1)
	go func() { session <- a.Session("live") }()
	select {
	case got := <-session:
		if got == nil {
			t.Fatal("live session disappeared")
		}
	case <-time.After(time.Second):
		t.Fatal("audit disk I/O blocked session lookup")
	}
}

func TestFinalAuditStoreUncertainPublicationDoesNotDuplicateOnRetry(t *testing.T) {
	a := NewAuth(t.TempDir())
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	original := syncStateDirectory
	syncStateDirectory = func(string) error { return errors.New("private filesystem path must not be exposed") }
	a.Append(AuditEntry{TS: 1, Action: "accepted"})
	syncStateDirectory = original
	if strings.Contains(a.AuditHealth()["last_error"].(string), "private filesystem") {
		t.Fatal("private storage error exposed")
	}
	a.Append(AuditEntry{TS: 2, Action: "next"})
	b := NewAuth(a.dataDir)
	if err := b.Load(); err != nil {
		t.Fatal(err)
	}
	if got := b.Audit(0); len(got) != 2 || got[0].TS != 2 || got[1].TS != 1 {
		t.Fatalf("uncertain publication duplicated/dropped event: %v", got)
	}
}

func TestFinalAuditStoreRedactsCredentialsAndRepairsIncompleteTail(t *testing.T) {
	a := NewAuth(t.TempDir())
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	a.Append(AuditEntry{Action: "settings", Detail: `rcon.password=private-one token="private two" Authorization: Bearer private-three ` + "tpa_" + strings.Repeat("a", 48)})
	for _, secret := range []string{"private-one", "private two", "private-three", strings.Repeat("a", 48)} {
		if strings.Contains(a.Audit(1)[0].Detail, secret) {
			t.Fatalf("credential leaked: %s", secret)
		}
	}
	f, err := os.OpenFile(a.auditSegmentPath(1), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"partial":`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b := NewAuth(a.dataDir)
	if err := b.Load(); err != nil {
		t.Fatal(err)
	}
	if len(b.Audit(0)) != 1 || b.AuditHealth()["recovered_incomplete_tails"] != uint64(1) {
		t.Fatalf("tail repair hid uncertainty: %v", b.AuditHealth())
	}
}

func TestFinalAuditStoreCorruptUsersNeverBecomeFreshSetup(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "users.json")
	body := []byte(`[null]`)
	if err := os.WriteFile(source, body, 0644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		a := NewAuth(dir)
		if err := a.Load(); err == nil {
			t.Fatal("corrupt authoritative users became fresh startup")
		}
		got, err := os.ReadFile(source)
		if err != nil || string(got) != string(body) {
			t.Fatal("authoritative corrupt users destroyed")
		}
	}
	backups, _ := filepath.Glob(source + ".corrupt-*")
	if len(backups) != 1 {
		t.Fatalf("corrupt source not safely preserved: %v", backups)
	}
}

func TestFinalAuditStoreCrashReportsLostRetryBacklog(t *testing.T) {
	a := NewAuth(t.TempDir())
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(a.auditSegmentPath(1), 0700); err != nil {
		t.Fatal(err)
	}
	a.Append(AuditEntry{TS: 1, Action: "must-audit"})
	if err := os.Remove(a.auditSegmentPath(1)); err != nil {
		t.Fatal(err)
	}
	// A new process cannot resurrect unpersisted in-memory entries, but the
	// durable health record must account for them rather than claim a healthy log.
	restarted := NewAuth(a.dataDir)
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if restarted.AuditHealth()["dropped_records"] != uint64(1) || restarted.AuditHealth()["healthy"] != false {
		t.Fatalf("lost backlog hidden: %v", restarted.AuditHealth())
	}
}

func TestFinalAuditStoreInterruptedMigrationRetainsSourceAndRetries(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`[{"ts":1,"actor":"legacy","action":"start"}]`)
	if err := os.WriteFile(filepath.Join(dir, "audit.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	original := syncStateDirectory
	syncStateDirectory = func(string) error { return errors.New("durability unavailable") }
	a := NewAuth(dir)
	loadErr := a.Load()
	syncStateDirectory = original
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if a.AuditHealth()["healthy"] != false {
		t.Fatal("interrupted migration reported healthy")
	}
	b := NewAuth(dir)
	if err := b.Load(); err != nil {
		t.Fatal(err)
	}
	b.Append(AuditEntry{TS: 2, Action: "new"})
	if got := b.Audit(0); len(got) != 2 || got[1].Actor != "legacy" {
		t.Fatalf("retry migration lost or duplicated source: %v", got)
	}
	source, err := os.ReadFile(filepath.Join(dir, "audit.json"))
	if err != nil || string(source) != string(body) {
		t.Fatal("migration destroyed authoritative legacy evidence")
	}
}
