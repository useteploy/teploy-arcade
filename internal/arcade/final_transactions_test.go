package arcade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func finalTransactionManager(t *testing.T) *Manager {
	t.Helper()
	httpServer, m := newTestAgent(t)
	t.Cleanup(httpServer.Close)
	t.Cleanup(m.Close)
	return m
}
func finalFailServerStore(t *testing.T, m *Manager) func() {
	t.Helper()
	old, err := os.ReadFile(m.storePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.storePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(m.storePath(), 0700); err != nil {
		t.Fatal(err)
	}
	fixed := false
	repair := func() {
		if fixed {
			return
		}
		fixed = true
		if err := os.Remove(m.storePath()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(m.storePath(), old, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(repair)
	return repair
}
func finalJournalFixture(t *testing.T, m *Manager, s *Server, phase string, original, names []string) string {
	t.Helper()
	staging := filepath.Join(m.serverDir(s), ".arcade-restore-transaction-fixture")
	if err := os.MkdirAll(filepath.Join(staging, "old"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeRestoreJournal(staging, restoreJournal{Phase: phase, Original: original, New: names, OldPort: s.Port, OldProps: maps.Clone(s.Props)}); err != nil {
		t.Fatal(err)
	}
	return staging
}

// Recovery must remain retryable when files rolled back but servers.json cannot
// be published. A second recovery must not mistake returned originals for new
// archive entries and delete them.
func TestFinalTransactionRecoveryPersistenceFailureRetainsRetryEvidence(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	dir := m.serverDir(s)
	staging := finalJournalFixture(t, m, s, "installing", []string{"world-original"}, []string{"world-original", "new-only"})
	if err := os.WriteFile(filepath.Join(staging, "old", "world-original"), []byte("original bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "world-original"), []byte("replacement bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new-only"), []byte("new bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	originalPort := s.Port
	s.mu.Lock()
	s.Port += 100
	s.Props["server-port"] = strconv.Itoa(s.Port)
	s.mu.Unlock()
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	repair := finalFailServerStore(t, m)
	m.recoverInterruptedRestores()
	if _, err := os.Stat(staging); err != nil {
		t.Errorf("persistence failure discarded rollback journal: %v", err)
	}
	s.mu.Lock()
	reason := s.RecoveryRequired
	s.mu.Unlock()
	if reason == "" {
		t.Error("persistence failure did not block lifecycle until metadata recovery")
	}
	if got, err := os.ReadFile(filepath.Join(dir, "world-original")); err != nil || string(got) != "original bytes" {
		t.Fatalf("first rollback lost originals: %q %v", got, err)
	}
	repair()
	m.recoverInterruptedRestores()
	if got, err := os.ReadFile(filepath.Join(dir, "world-original")); err != nil || string(got) != "original bytes" {
		t.Fatalf("retry deleted already returned originals: %q %v", got, err)
	}
	s.mu.Lock()
	port, reason := s.Port, s.RecoveryRequired
	s.mu.Unlock()
	if port != originalPort || reason != "" {
		t.Fatalf("retry metadata port=%d recovery=%q", port, reason)
	}
	disk, err := os.ReadFile(m.storePath())
	if err != nil {
		t.Fatal(err)
	}
	var servers []*Server
	if err := json.Unmarshal(disk, &servers); err != nil {
		t.Fatal(err)
	}
	for _, saved := range servers {
		if saved.ID == s.ID && saved.Port != originalPort {
			t.Fatalf("retry failed to publish original metadata: %d", saved.Port)
		}
	}
}

func TestFinalTransactionCommittedRecoveryClearsPersistedBarrier(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	dir := m.serverDir(s)
	staging := finalJournalFixture(t, m, s, "installing", nil, []string{"world"})
	os.WriteFile(filepath.Join(dir, "world"), []byte("committed bytes"), 0600)
	if err := writeDurable(filepath.Join(staging, ".committed"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.RecoveryRequired = "commit durability uncertain"
	s.mu.Unlock()
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	m.recoverInterruptedRestores()
	s.mu.Lock()
	reason := s.RecoveryRequired
	s.mu.Unlock()
	if reason != "" {
		t.Fatalf("authoritative committed recovery left server permanently blocked: %q", reason)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "world")); err != nil || string(got) != "committed bytes" {
		t.Fatalf("committed recovery changed world: %q %v", got, err)
	}
	disk, _ := os.ReadFile(m.storePath())
	var saved []*Server
	json.Unmarshal(disk, &saved)
	for _, record := range saved {
		if record.ID == s.ID && record.RecoveryRequired != "" {
			t.Fatal("recovery barrier remained durable")
		}
	}
}

func TestFinalTransactionPartialEvacuationPreservesUnmovedOriginals(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	dir := m.serverDir(s)
	staging := finalJournalFixture(t, m, s, "evacuating", []string{"a-moved", "b-unmoved"}, []string{"a-moved", "b-unmoved", "new-only"})
	os.WriteFile(filepath.Join(staging, "old", "a-moved"), []byte("moved original"), 0600)
	os.WriteFile(filepath.Join(dir, "b-unmoved"), []byte("unmoved original"), 0600)
	m.recoverInterruptedRestores()
	for name, want := range map[string]string{"a-moved": "moved original", "b-unmoved": "unmoved original"} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Fatalf("evacuation recovery %s: %q %v", name, got, err)
		}
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("successful evacuation recovery left staging")
	}
}

func TestFinalTransactionDeletionTombstonePreventsCrashResurrection(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	if err := m.beginDeletion(s, false, false); err != nil {
		t.Fatal(err)
	}
	// Simulate crash after durable intent and before servers.json removal.
	next := NewManager(m.dataDir, NewHub())
	t.Cleanup(next.Close)
	if err := next.Load(); err != nil {
		t.Fatal(err)
	}
	if next.Get(s.ID) != nil {
		t.Fatal("durable deletion intent resurrected the server")
	}
	if _, err := os.Stat(m.serverDir(s)); err != nil {
		t.Fatal("preserve-data intent deleted owned world", err)
	}
	if _, err := os.Stat(m.deletionPath(s.ID)); !os.IsNotExist(err) {
		t.Fatal("completed persistent detach retained tombstone")
	}
	disk, _ := os.ReadFile(m.storePath())
	var saved []*Server
	json.Unmarshal(disk, &saved)
	for _, record := range saved {
		if record.ID == s.ID {
			t.Fatal("resurrection remains in servers.json")
		}
	}
}

func TestFinalTransactionSettingsAndResourcesRollbackOnPersistenceFailure(t *testing.T) {
	for _, operation := range []string{"settings", "resources", "retention", "order"} {
		t.Run(operation, func(t *testing.T) {
			m := finalTransactionManager(t)
			s := m.List()[0]
			s.mu.Lock()
			oldPort, oldMemory, oldCPU, oldKeep := s.Port, s.MemoryMB, s.CPU, s.BackupKeep
			oldProps := maps.Clone(s.Props)
			oldPending := append([]string(nil), s.PendingRestart...)
			s.mu.Unlock()
			oldOrder := append([]string(nil), m.order...)
			propsPath := filepath.Join(m.serverDir(s), "server.properties")
			oldBytes, _ := os.ReadFile(propsPath)
			finalFailServerStore(t, m)
			var err error
			switch operation {
			case "settings":
				_, err = m.ApplySettingsConditional(s, map[string]string{"motd": "changed fixture"}, settingsETag(s))
			case "resources":
				_, err = m.SetResources(s, oldMemory+128, oldCPU+0.1)
			case "retention":
				err = m.SetBackupKeep(s, oldKeep+1, "fixture")
			case "order":
				order := append([]string(nil), oldOrder...)
				for l, r := 0, len(order)-1; l < r; l, r = l+1, r-1 {
					order[l], order[r] = order[r], order[l]
				}
				err = m.Reorder(order)
			}
			if err == nil {
				t.Fatal("persistence failure reported success")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.Port != oldPort || s.MemoryMB != oldMemory || s.CPU != oldCPU || s.BackupKeep != oldKeep || !reflect.DeepEqual(s.Props, oldProps) || !reflect.DeepEqual(s.PendingRestart, oldPending) {
				t.Fatal("failed operation leaked model candidate")
			}
			if !reflect.DeepEqual(m.order, oldOrder) {
				t.Fatal("failed reorder leaked candidate")
			}
			if operation == "settings" {
				got, _ := os.ReadFile(propsPath)
				if string(got) != string(oldBytes) {
					t.Fatal("settings rollback failed to restore file")
				}
			}
		})
	}
}

func TestFinalTransactionStaleETagAndCursorPreserveNewerState(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	if err := m.WriteFile(s, "notes.txt", "original"); err != nil {
		t.Fatal(err)
	}
	etag := contentETag("original")
	if err := m.WriteFileConditional(s, "notes.txt", "newer", etag); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFileConditional(s, "notes.txt", "stale", etag); !errors.Is(err, errEditConflict) {
		t.Fatal("stale write accepted", err)
	}
	if got, err := m.ReadFile(s, "notes.txt"); err != nil || got != "newer" {
		t.Fatal("stale write lost newer bytes", got, err)
	}
	dir := filepath.Join(m.serverDir(s), "paged")
	os.Mkdir(dir, 0700)
	for i := 0; i < maxListItems+1; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%05d.txt", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	page, cursor, err := m.ListFilesPage(context.Background(), s, "paged", "")
	if err != nil || len(page) != maxListItems || cursor == "" {
		t.Fatalf("initial page %d %q %v", len(page), cursor, err)
	}
	os.WriteFile(filepath.Join(dir, "new-file.txt"), nil, 0600)
	if _, _, err := m.ListFilesPage(context.Background(), s, "paged", cursor); !errors.Is(err, errStaleCursor) {
		t.Fatal("mutated listing reused cursor", err)
	}
}

func TestFinalTransactionPartialRollbackPreservesReturnedOriginals(t *testing.T) {
	for _, phase := range []string{"installing", "rolling_back"} {
		t.Run(phase, func(t *testing.T) {
			m := finalTransactionManager(t)
			s := m.List()[0]
			dir := m.serverDir(s)
			staging := finalJournalFixture(t, m, s, phase, []string{"a-returned", "b-held"}, []string{"a-returned", "b-held", "new-only"})
			os.WriteFile(filepath.Join(dir, "a-returned"), []byte("returned original"), 0600)
			os.WriteFile(filepath.Join(dir, "b-held"), []byte("installed replacement"), 0600)
			os.WriteFile(filepath.Join(staging, "old", "b-held"), []byte("held original"), 0600)
			os.WriteFile(filepath.Join(dir, "new-only"), []byte("installed new bytes"), 0600)
			m.recoverInterruptedRestores()
			for name, want := range map[string]string{"a-returned": "returned original", "b-held": "held original"} {
				if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
					t.Fatalf("partial rollback %s: %q %v", name, got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "new-only")); !os.IsNotExist(err) {
				t.Fatal("new-only namespace survived rollback")
			}
		})
	}
}

func TestFinalTransactionLegacyRecoveryPreservesAmbiguousUnheldNamespace(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	dir := m.serverDir(s)
	staging := filepath.Join(dir, ".arcade-restore-legacy-fixture")
	os.MkdirAll(filepath.Join(staging, "old"), 0700)
	os.WriteFile(filepath.Join(staging, "old", "held"), []byte("old original"), 0600)
	os.WriteFile(filepath.Join(dir, "held"), []byte("new replacement"), 0600)
	os.WriteFile(filepath.Join(dir, "unmoved"), []byte("unmoved original"), 0600)
	m.recoverInterruptedRestores()
	for name, want := range map[string]string{"held": "old original", "unmoved": "unmoved original"} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Fatalf("legacy recovery %s: %q %v", name, got, err)
		}
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("legacy successful recovery retained staging")
	}
}

// A crash after restored properties were adopted must roll back derived model
// fields too, so panel capacity does not disagree with the original world.
func TestFinalTransactionRecoveryRestoresMaxPlayers(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	s.mu.Lock()
	s.MaxPlayers = 17
	s.Props["max-players"] = "17"
	s.mu.Unlock()
	staging := finalJournalFixture(t, m, s, "installing", []string{"world"}, []string{"world"})
	if err := os.WriteFile(filepath.Join(staging, "old", "world"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.MaxPlayers = 91
	s.Props["max-players"] = "91"
	s.mu.Unlock()
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	m.recoverInterruptedRestores()
	s.mu.Lock()
	max, prop := s.MaxPlayers, s.Props["max-players"]
	s.mu.Unlock()
	if max != 17 || prop != "17" {
		t.Fatalf("rolled-back model max=%d props=%s, want 17", max, prop)
	}
	disk, err := os.ReadFile(m.storePath())
	if err != nil {
		t.Fatal(err)
	}
	var saved []*Server
	if err := json.Unmarshal(disk, &saved); err != nil {
		t.Fatal(err)
	}
	for _, entry := range saved {
		if entry.ID == s.ID && entry.MaxPlayers != 17 {
			t.Fatalf("persisted rolled-back max=%d, want 17", entry.MaxPlayers)
		}
	}
}

// A fully installed archive cannot be marked committed when its model failed
// to persist. Retained originals must recover the pre-request generation.
func TestFinalTransactionRestoreMetadataFailureRetainsOriginalGeneration(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	if err := m.WriteFile(s, "world/important.txt", "archive generation"); err != nil {
		t.Fatal(err)
	}
	b, err := m.CreateBackup(s, "fixture", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile(s, "world/important.txt", "current generation"); err != nil {
		t.Fatal(err)
	}
	repair := finalFailServerStore(t, m)
	if err := m.RestoreBackup(s, b.ID, "tester"); err == nil {
		t.Fatal("restore reported success despite model publication failure")
	}
	s.mu.Lock()
	reason := s.RecoveryRequired
	s.mu.Unlock()
	if reason == "" {
		t.Fatal("metadata failure did not retain lifecycle interlock")
	}
	dirs, err := filepath.Glob(filepath.Join(m.serverDir(s), ".arcade-restore-*"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("missing unique recovery journal: %v %v", dirs, err)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], ".committed")); !os.IsNotExist(err) {
		t.Fatalf("failed publication marked committed: %v", err)
	}
	held, err := os.ReadFile(filepath.Join(dirs[0], "old", "world", "important.txt"))
	if err != nil || string(held) != "current generation" {
		t.Fatalf("lost pre-restore generation: %q %v", held, err)
	}
	repair()
	m.recoverInterruptedRestores()
	live, err := m.ReadFile(s, "world/important.txt")
	if err != nil || live != "current generation" {
		t.Fatalf("recovery failed original generation: %q %v", live, err)
	}
}
