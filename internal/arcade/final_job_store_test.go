package arcade

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func jobStoreFixture(t *testing.T) (*Manager, *importJob) {
	t.Helper()
	m := &Manager{dataDir: filepath.Join(t.TempDir(), "state"), ctx: context.Background()}
	j := newImportJob(&ImportScan{Path: t.TempDir(), SizeBytes: 100}, ImportCopy, "Fixture")
	t.Cleanup(func() { importJobs.mu.Lock(); delete(importJobs.jobs, j.pub.ID); importJobs.mu.Unlock() })
	return m, j
}

func TestJobStoreRecoversInterruptedWithoutDeletingWorld(t *testing.T) {
	m, j := jobStoreFixture(t)
	if err := m.trackImportJob(j); err != nil {
		t.Fatal(err)
	}
	j.advance(42)
	if err := j.persist(); err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(m.dataDir, "servers", "partial", "world.dat")
	if err := os.MkdirAll(filepath.Dir(world), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(world, []byte("irreplaceable partial bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.loadImportJobs(); err != nil {
		t.Fatal(err)
	}
	recovered, ok := importJobByID(j.pub.ID)
	if !ok {
		t.Fatal("recovered job absent")
	}
	v := recovered.view()
	if v.State != ImportFailed || v.Copied != 42 || !strings.Contains(v.Error, "interrupted") || v.Finished == 0 {
		t.Fatalf("recovery: %+v", v)
	}
	b, err := os.ReadFile(world)
	if err != nil || string(b) != "irreplaceable partial bytes" {
		t.Fatal("partial world changed", err)
	}
	if err := m.loadImportJobs(); err != nil {
		t.Fatal(err)
	}
	again, _ := importJobByID(j.pub.ID)
	if again.view().Finished != v.Finished {
		t.Fatal("restart renewed terminal retention")
	}
}

func TestJobStoreCompletedResultAndTTLRetention(t *testing.T) {
	m, j := jobStoreFixture(t)
	if err := m.trackImportJob(j); err != nil {
		t.Fatal(err)
	}
	j.done("server-fixture")
	if err := j.persist(); err != nil {
		t.Fatal(err)
	}
	if err := m.loadImportJobs(); err != nil {
		t.Fatal(err)
	}
	current, _ := importJobByID(j.pub.ID)
	if current.view().State != ImportDone || current.view().ServerID != "server-fixture" {
		t.Fatal("terminal identity lost")
	}
	j.mu.Lock()
	j.pub.Started = time.Now().Add(-3 * time.Hour).Unix()
	j.pub.Finished = time.Now().Add(-2 * time.Hour).Unix()
	j.mu.Unlock()
	if err := j.persist(); err != nil {
		t.Fatal(err)
	}
	if err := m.loadImportJobs(); err != nil {
		t.Fatal(err)
	}
	if _, ok := importJobByID(j.pub.ID); ok {
		t.Fatal("expired record remained pollable")
	}
	if _, err := os.Stat(j.storePath); !os.IsNotExist(err) {
		t.Fatal("expired metadata not removed", err)
	}
}

func TestJobStoreCorruptEvidenceFailsClosedBeforeRecovery(t *testing.T) {
	m, j := jobStoreFixture(t)
	if err := m.trackImportJob(j); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(j.storePath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(m.dataDir, "jobs", "broken.json")
	evidence := []byte("{broken confidential evidence")
	if err := os.WriteFile(corrupt, evidence, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.loadImportJobs(); err == nil {
		t.Fatal("corrupt registry admitted")
	}
	after, err := os.ReadFile(j.storePath)
	if err != nil || string(after) != string(before) {
		t.Fatal("validated record changed before whole registry validation")
	}
	got, _ := os.ReadFile(corrupt)
	if string(got) != string(evidence) {
		t.Fatal("corrupt evidence changed")
	}
}

func TestJobStoreStrictBoundedRecords(t *testing.T) {
	cases := map[string]func([]byte) []byte{
		"oversize":        func(b []byte) []byte { return []byte(strings.Repeat(" ", durableJobBytes+1)) },
		"duplicate":       func(b []byte) []byte { return []byte(strings.Replace(string(b), `"id":`, `"id":"duplicate","id":`, 1)) },
		"unknown":         func(b []byte) []byte { return append([]byte(`{"unknown":true,`), b[1:]...) },
		"trailing":        func(b []byte) []byte { return append(b, []byte(`{}`)...) },
		"null":            func(b []byte) []byte { return []byte(`null`) },
		"missingidentity": func(b []byte) []byte { return []byte(`{"id":"fixture"}`) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m, j := jobStoreFixture(t)
			if err := m.trackImportJob(j); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(j.storePath)
			b = change(b)
			if err := os.WriteFile(j.storePath, b, 0600); err != nil {
				t.Fatal(err)
			}
			if err := m.loadImportJobs(); err == nil {
				t.Fatal("invalid record admitted")
			}
			got, _ := os.ReadFile(j.storePath)
			if string(got) != string(b) {
				t.Fatal("invalid evidence changed")
			}
		})
	}
}

func TestJobStoreRejectsSymlinkedRecordsAndDirectory(t *testing.T) {
	m, j := jobStoreFixture(t)
	if err := m.trackImportJob(j); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(m.dataDir, "jobs", "outside.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := m.loadImportJobs(); err == nil {
		t.Fatal("symlink record admitted")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	other := &Manager{dataDir: t.TempDir()}
	if err := os.Symlink(filepath.Dir(target), filepath.Join(other.dataDir, "jobs")); err != nil {
		t.Fatal(err)
	}
	if err := other.loadImportJobs(); err == nil {
		t.Fatal("symlink registry admitted")
	}
}

func TestJobStoreAdmissionCapAndFailedProvisionalCleanup(t *testing.T) {
	m, j := jobStoreFixture(t)
	if err := os.MkdirAll(filepath.Join(m.dataDir, "jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < durableJobLimit; i++ {
		v := j.view()
		v.ID = strings.Repeat("a", 1) + time.Unix(int64(i+1), 0).Format("150405")
		// Numeric IDs remain unique across this bounded 1000-second window.
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(m.dataDir, "jobs", v.ID+".json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.trackImportJob(j); err == nil {
		t.Fatal("capacity exceeded")
	}
	if _, ok := importJobByID(j.pub.ID); ok {
		t.Fatal("failed admission left provisional pollable job")
	}
}

func TestJobStoreConcurrentPersistenceKeepsTerminalState(t *testing.T) {
	m, j := jobStoreFixture(t)
	if err := m.trackImportJob(j); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 12; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := j.persist(); err != nil {
				t.Error(err)
			}
		}()
	}
	j.done("server-fixture")
	workers.Wait()
	if err := j.persist(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(j.storePath)
	var v ImportJob
	if err := json.Unmarshal(b, &v); err != nil || v.State != ImportDone {
		t.Fatal("terminal state overwritten", err)
	}
}

func TestJobStoreOperationModesAndFingerprint(t *testing.T) {
	for _, mode := range []string{"backup", "plugin-install", "restore"} {
		t.Run(mode, func(t *testing.T) {
			m, j := jobStoreFixture(t)
			j.pub.Mode = mode
			j.pub.ServerID = "server-fixture"
			j.pub.RequestHash = strings.Repeat("a", 64)
			if err := m.trackImportJob(j); err != nil {
				t.Fatal(err)
			}
			if err := m.loadImportJobs(); err != nil {
				t.Fatal(err)
			}
		})
	}
	m, j := jobStoreFixture(t)
	j.pub.RequestHash = "invalid"
	if err := m.trackImportJob(j); err == nil {
		t.Fatal("invalid request fingerprint admitted")
	}
}

func TestJobStoreReaperKeepsActiveJobsAndExpiresOnlyMetadata(t *testing.T) {
	m, j := jobStoreFixture(t)
	if err := m.trackImportJob(j); err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	j.pub.Started = time.Now().Add(-3 * time.Hour).Unix()
	j.mu.Unlock()
	if err := j.persist(); err != nil {
		t.Fatal(err)
	}
	if err := m.reapImportJobs(); err != nil {
		t.Fatal(err)
	}
	if _, ok := importJobByID(j.pub.ID); !ok {
		t.Fatal("active job evicted by reaper")
	}
	j.mu.Lock()
	j.pub.State = ImportFailed
	j.pub.Error = "fixture"
	j.pub.Finished = time.Now().Add(-2 * time.Hour).Unix()
	j.mu.Unlock()
	if err := j.persist(); err != nil {
		t.Fatal(err)
	}
	if err := m.reapImportJobs(); err != nil {
		t.Fatal(err)
	}
	if _, ok := importJobByID(j.pub.ID); ok {
		t.Fatal("expired terminal job retained")
	}
	if _, err := os.Stat(j.pub.Path); err != nil {
		t.Fatal("source directory changed", err)
	}
}
