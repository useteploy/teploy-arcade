package arcade

import (
	"strings"
	"sync"
	"testing"
)

// H7. A list change on a running server is composed into one console line, and
// a game console reads one command per line. Before the fix only `who` was
// checked, and only on the add path: a ban reason of "cheating\nop attacker"
// banned the griefer and then made the attacker an operator, which is not a
// power an operator is meant to have.
func TestPlayerListArgsCannotSmuggleASecondConsoleCommand(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	for _, reason := range []string{
		"cheating\nop attacker",
		"cheating\r\nwhitelist off",
		"cheating\x00stop",
	} {
		if err := mgr.AddToList(s, ListBanned, "Griefer", reason, "tester"); err == nil {
			t.Errorf("reason %q accepted; everything after the break runs as its own command", reason)
		}
	}
	if e, err := mgr.readList(s, ListBanned); err != nil || len(e) != 0 {
		t.Errorf("a refused ban still touched the file: %v (%v)", e, err)
	}

	// The name reaches the same line, on both paths. Remove validated nothing at
	// all, so it is seeded here as a hand-edited ops.json would deliver it -
	// otherwise "not on the list" hides the missing check.
	if err := mgr.AddToList(s, ListOps, "Griefer\nop attacker", "", "tester"); err == nil {
		t.Error("a name containing a newline was accepted by AddToList")
	}
	if err := mgr.writeList(s, ListOps, []ListEntry{{Name: "Griefer\nop attacker", Level: 4}}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RemoveFromList(s, ListOps, "Griefer\nop attacker", "tester"); err == nil {
		t.Error("a name containing a newline was accepted by RemoveFromList")
	}

	// A real ban still works, and the composed command stays on one line.
	if err := mgr.AddToList(s, ListBanned, "Griefer", "griefing spawn", "tester"); err != nil {
		t.Fatalf("a legitimate ban was refused: %v", err)
	}
	if cmd := gameCommand(ListBanned, true, "Griefer", "griefing spawn"); strings.ContainsAny(cmd, "\r\n") {
		t.Errorf("composed command spans more than one line: %q", cmd)
	}
}

// M4. Run had no per-task guard. The loop reads LastRun before spawning and
// LastRun is only written at the end of the run, so an operator pressing
// "Run now" inside that window gets a second copy of the task: two lifecycle
// calls on one server, or two interleaved warn-then-restart streams.
func TestSchedulerRunIsNotReentrant(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	task, err := mgr.sched.Add(&Task{
		ServerID: s.ID, Name: "nightly", Commands: "!wait 1", Time: "04:00", Repeat: true,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- mgr.sched.Run(task.ID, "tester")
		}()
	}
	wg.Wait()
	close(errs)

	refused := 0
	for err := range errs {
		if err != nil {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("%d of 2 concurrent runs were refused, want exactly 1", refused)
	}
	if got := mgr.sched.Get(task.ID).Runs; got != 1 {
		t.Errorf("task recorded %d runs; the second caller executed it again", got)
	}
}

// M6. Run threw away the error from its final Update, and Update refused to
// persist anything when the task's stored Time did not parse. A time
// hand-edited into garbage in tasks.json therefore meant a task that ran but
// recorded nothing: no LastRun, no LastErr in the UI, and a one-shot that never
// disabled itself.
func TestSchedulerRecordsARunWhenTheStoredClockIsGarbage(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	task, err := mgr.sched.Add(&Task{
		ServerID: s.ID, Name: "one shot", Commands: "!wait 0", Time: "04:00", Repeat: false,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	// What loading a hand-edited tasks.json leaves behind.
	mgr.sched.mu.Lock()
	for _, tk := range mgr.sched.tasks {
		if tk.ID == task.ID {
			tk.Time = "noon"
		}
	}
	mgr.sched.mu.Unlock()

	if err := mgr.sched.Run(task.ID, "tester"); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := mgr.sched.Get(task.ID)
	if got.Runs != 1 || got.LastRun == 0 {
		t.Errorf("run not recorded (%+v); the next tick fires it again", got)
	}
	if got.Enabled {
		t.Error("a one-shot stayed enabled because its result was never written back")
	}
}

// M6, second half. Update applies the caller's mutation before validating the
// clock, so a rejected edit used to be left on the live task: the API returned
// an error while the in-memory task kept the bad time and stopped firing.
func TestSchedulerRejectedTimeEditDoesNotSurviveInMemory(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]

	task, err := mgr.sched.Add(&Task{
		ServerID: s.ID, Name: "nightly", Commands: "!wait 0", Time: "04:00", Repeat: true,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := mgr.sched.Update(task.ID, func(t *Task) { t.Time = "99:99" }); err == nil {
		t.Fatal("an invalid time was accepted")
	}
	if got := mgr.sched.Get(task.ID).Time; got != "04:00" {
		t.Errorf("task time is %q after a rejected edit, want the original 04:00", got)
	}
}

// L1. atoi returns 0 for anything non-numeric, so every one of these parsed as
// a valid time - most of them as midnight. A typo'd task time fired at 00:00
// rather than being refused when it was entered.
func TestParseClockRejectsNonNumericFields(t *testing.T) {
	for _, bad := range []string{"foo:bar", "1a:00", "04:0x", ":", "::", "04:", "-1:00", "0400:00"} {
		if secs, err := parseClock(bad); err == nil {
			t.Errorf("%q accepted as %d seconds past midnight", bad, secs)
		}
	}
	for _, good := range []string{"04:00", "4:5", "23:59:59", " 07:30 "} {
		if _, err := parseClock(good); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
}
