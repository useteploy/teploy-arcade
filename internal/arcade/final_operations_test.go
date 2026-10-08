package arcade

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestFinalOperationAdmissionIsDurableIdempotentAndCancellationOwned(t *testing.T) {
	httpServer, m := newTestAgent(t)
	defer httpServer.Close()
	defer m.Close()
	s := m.List()[0]
	var calls atomic.Int32
	started := make(chan struct{})
	work := func(ctx context.Context) error { calls.Add(1); close(started); <-ctx.Done(); return ctx.Err() }
	first, err := m.startOperation(s, "backup", "tester", "key", "payload", work)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := m.startOperation(s, "backup", "tester", "key", "payload", work)
	if err != nil || first.ID != second.ID {
		t.Fatalf("duplicate admission %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate side effect")
	}
	if _, err := m.startOperation(s, "backup", "tester", "key", "changed", work); err == nil {
		t.Fatal("changed payload reused key")
	}
	if _, err := os.Stat(filepath.Join(m.dataDir, "jobs", first.ID+".json")); err != nil {
		t.Fatal("202 precedes durable ownership", err)
	}
	m.Close()
	job, _ := importJobByID(first.ID)
	if job.view().State != ImportFailed {
		t.Fatal("cancellation terminal state missing")
	}
}
func TestFinalArchiveCancellationNeverPublishes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	os.Mkdir(source, 0700)
	os.WriteFile(filepath.Join(source, "world"), []byte("world"), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := filepath.Join(root, "backup.tar.gz")
	if _, err := tarGzContext(ctx, source, target); err == nil {
		t.Fatal("cancelled archive succeeded")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("cancelled archive published")
	}
}
func TestFinalDockerExecutorOutputAndAdmissionBound(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "docker")
	os.WriteFile(path, []byte("#!/bin/sh\ncase \"$1\" in large) head -c 1100000 /dev/zero;; wait) sleep 30;; esac\n"), 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := dockerOutput(context.Background(), time.Second, true, "large")
	if err == nil || len(out) > 1<<20 {
		t.Fatal("unbounded output", len(out), err)
	}
	for i := 0; i < cap(dockerCommandSlots); i++ {
		dockerCommandSlots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = dockerOutput(ctx, time.Second, false, "probe")
	for i := 0; i < cap(dockerCommandSlots); i++ {
		<-dockerCommandSlots
	}
	if err == nil {
		t.Fatal("admission ignored deadline")
	}
}
func TestFinalDockerDaemonCapacityDistinctFromPanel(t *testing.T) {
	daemonCapacity.Lock()
	oldCPU, oldMem := daemonCapacity.CPUs, daemonCapacity.MemoryMB
	daemonCapacity.CPUs = 1
	daemonCapacity.MemoryMB = 1024
	daemonCapacity.Unlock()
	defer func() {
		daemonCapacity.Lock()
		daemonCapacity.CPUs = oldCPU
		daemonCapacity.MemoryMB = oldMem
		daemonCapacity.Unlock()
	}()
	if checkFitsDaemon(RuntimeDocker, 2048, 1) == nil || checkFitsDaemon(RuntimeDocker, 512, 2) == nil {
		t.Fatal("daemon capacity ignored")
	}
	if checkFitsDaemon(RuntimeSim, 2048, 2) != nil {
		t.Fatal("simulation attributed daemon capacity")
	}
}

func TestFinalPropertiesRejectAlternateManagedIdentity(t *testing.T) {
	for _, body := range []string{"server\\u002dport=25566\n", "server-port 25566\n", "server-port:25566\n", "motd=hello\\\nserver-port=25566\n"} {
		if _, _, err := propsPortValue(body); err == nil {
			t.Fatalf("accepted ambiguous Java syntax %q", body)
		}
	}
	if err := validateCanonicalProps("motd=hello = world  \ncustom=\\u00a7text\nserver-port=25566\n"); err != nil {
		t.Fatal(err)
	}
	if got := parsePropsLines("motd=hello  \n")["motd"]; got != "hello  " {
		t.Fatalf("trailing value bytes lost %q", got)
	}
}

func TestFinalLifecycleIntentFailureDoesNotLaunchOrLosePreviousIntent(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	repair := finalFailServerStore(t, m)
	previous := s.IntentGeneration
	if err := m.Start(s.ID); err == nil {
		t.Fatal("start acknowledged unrecorded intent")
	}
	if s.State() != StatusStopped || s.DesiredRunning != nil || s.IntentGeneration != previous {
		t.Fatal("failed intent changed durable model")
	}
	repair()
	generation, err := m.persistIntent(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.persistIntent(s, false); err != nil {
		t.Fatal(err)
	}
	if err := m.startExpected(s.ID, generation); err == nil {
		t.Fatal("superseded start launched")
	}
	if s.State() != StatusStopped {
		t.Fatal("superseded intent changed runtime")
	}
}

func TestFinalCopyRejectsChangedSourceDirectoryAndDoesNotCopySessionLock(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	dst := filepath.Join(root, "copy")
	os.Mkdir(source, 0700)
	os.Mkdir(filepath.Join(source, "world"), 0700)
	os.WriteFile(filepath.Join(source, "world", "session.lock"), []byte("game-lock"), 0600)
	os.WriteFile(filepath.Join(source, "world", "level.dat"), []byte("world"), 0600)
	info, _ := os.Stat(source)
	job := newImportJob(&ImportScan{Path: source, Name: "source"}, ImportCopy, "source")
	job.ctx = context.Background()
	job.sourceDirectory = info
	if err := copyTree(source, dst, job); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "world", "session.lock")); !os.IsNotExist(err) {
		t.Fatal("copy opened transient JVM lock file")
	}
	os.Rename(source, source+"-original")
	os.Mkdir(source, 0700)
	if err := copyTree(source, filepath.Join(root, "copy2"), job); err == nil {
		t.Fatal("changed source inode accepted")
	}
}

func TestFinalRecoveryValidatesWholeManifestAndConfinesHeldTree(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	dir := m.serverDir(s)
	stage := filepath.Join(dir, ".arcade-restore-invalid-manifest")
	os.MkdirAll(filepath.Join(stage, "old"), 0700)
	good := filepath.Join(dir, "keep-original")
	os.WriteFile(good, []byte("keep"), 0600)
	b := []byte(`{"phase":"installing","original":[],"new":["keep-original","../escape"],"old_port":25565,"old_props":{}}`)
	os.WriteFile(filepath.Join(stage, "journal.json"), b, 0600)
	m.recoverInterruptedRestores()
	if got, err := os.ReadFile(good); err != nil || string(got) != "keep" {
		t.Fatal("partial invalid manifest deleted original before validation")
	}
	if s.RecoveryRequired == "" {
		t.Fatal("invalid manifest did not arm interlock")
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside-original")
	os.WriteFile(outsideFile, []byte("outside"), 0600)
	stage2 := filepath.Join(dir, ".arcade-restore-held-escape")
	os.Mkdir(stage2, 0700)
	os.Symlink(outside, filepath.Join(stage2, "old"))
	m.recoverInterruptedRestores()
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != "outside" {
		t.Fatal("held symlink escaped managed root")
	}
}

func TestFinalTaskDisableCancelsQueuedOccurrenceButManualRunRemainsAvailable(t *testing.T) {
	m := finalTransactionManager(t)
	s := m.List()[0]
	task, err := m.sched.Add(&Task{ServerID: s.ID, Name: "queued", Time: "23:59", Commands: "!wait 0", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.sched.record(task.ID, func(task *Task) { task.PendingAt = time.Now().Unix(); task.RunState = "pending" }); err != nil {
		t.Fatal(err)
	}
	current, err := m.sched.Update(s.ID, task.ID, func(task *Task) { task.Enabled = false })
	if err != nil {
		t.Fatal(err)
	}
	if current.PendingAt != 0 || current.RunState != "cancelled" {
		t.Fatal("disabled occurrence remained queued")
	}
	if err := m.sched.Run(s.ID, task.ID, "manual"); err != nil {
		t.Fatal("explicit manual run rejected", err)
	}
}
