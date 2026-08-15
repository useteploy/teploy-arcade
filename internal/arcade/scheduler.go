package arcade

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Scheduler: timed tasks per server.
//
// The familiar shape is a console command at a 24h clock time, optionally daily.
// That shape is kept, plus panel-level actions, because the actual reason
// people use a scheduler on a Minecraft host is the nightly restart and the
// nightly backup — neither of which is a console command.
//
//	say Server restarts in 5 minutes    -> console command
//	!restart                            -> panel action
//	!backup nightly                     -> panel action
//
// Multiple steps separated by `;`, run in order, which is how you build the
// warn-then-restart pattern operators actually want:
//
//	say Restarting in 60s; !wait 60; !restart

type Task struct {
	ID       string `json:"id"`
	ServerID string `json:"server_id"`
	Name     string `json:"name"`
	Commands string `json:"commands"`
	Time     string `json:"time"` // HH:MM or HH:MM:SS, 24h, host local
	Repeat   bool   `json:"repeat"`
	Enabled  bool   `json:"enabled"`
	LastRun  int64  `json:"last_run"`
	LastErr  string `json:"last_err"`
	Runs     int    `json:"runs"`
}

type Scheduler struct {
	mu    sync.RWMutex
	path  string
	tasks []*Task
	mgr   *Manager
	seq   int

	// Separate from mu because a run holds it for the whole task — minutes, if
	// the task waits — while mu is taken and released repeatedly underneath.
	runMu   sync.Mutex
	running map[string]bool
}

func newScheduler(dataDir string, m *Manager) *Scheduler {
	s := &Scheduler{path: filepath.Join(dataDir, "tasks.json"), mgr: m, running: map[string]bool{}}
	if b, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(b, &s.tasks); err != nil {
			quarantine(s.path, err) // losing every task silently is worse
		}
	}
	return s
}

func (sc *Scheduler) save() error {
	b, err := json.MarshalIndent(sc.tasks, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(sc.path, b, 0o644)
}

// clockField parses one field of a 24h clock. It exists because atoi reports 0
// for anything that is not a number: without the digit check "foo:bar" is a
// perfectly valid midnight, so a typo'd time in tasks.json fires the task at
// 00:00 every night instead of being refused. The two-digit cap also keeps a
// long numeric field from wrapping atoi into an in-range hour.
func clockField(s string) (int, bool) {
	if len(s) == 0 || len(s) > 2 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	return atoi(s), true
}

// parseClock accepts HH:MM or HH:MM:SS and returns seconds since midnight.
func parseClock(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("time must be HH:MM or HH:MM:SS (24h)")
	}
	h, okH := clockField(parts[0])
	m, okM := clockField(parts[1])
	sec, okSec := 0, true
	if len(parts) == 3 {
		sec, okSec = clockField(parts[2])
	}
	if !okH || !okM || !okSec || h > 23 || m > 59 || sec > 59 {
		return 0, fmt.Errorf("%q is not a valid 24h time", s)
	}
	return h*3600 + m*60 + sec, nil
}

func (sc *Scheduler) List(serverID string) []*Task {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	out := []*Task{}
	for _, t := range sc.tasks {
		if serverID == "" || t.ServerID == serverID {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out
}

func (sc *Scheduler) Add(t *Task) (*Task, error) {
	if strings.TrimSpace(t.Name) == "" {
		return nil, fmt.Errorf("a task name is required")
	}
	if strings.TrimSpace(t.Commands) == "" {
		return nil, fmt.Errorf("at least one command is required")
	}
	if _, err := parseClock(t.Time); err != nil {
		return nil, err
	}
	if sc.mgr.Get(t.ServerID) == nil {
		return nil, fmt.Errorf("no such server")
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.seq++
	t.ID = fmt.Sprintf("t%d%02d", time.Now().Unix()%100000, sc.seq%100)
	t.Enabled = true
	sc.tasks = append(sc.tasks, t)
	return t, sc.save()
}

func (sc *Scheduler) Update(id string, fn func(*Task)) (*Task, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, t := range sc.tasks {
		if t.ID == id {
			before := *t
			fn(t)
			if _, err := parseClock(t.Time); err != nil {
				// A rejected edit must not survive in memory. Leaving the bad
				// time on the live task would stop the loop firing a task the
				// operator was just told was left unchanged.
				*t = before
				return nil, err
			}
			cp := *t
			return &cp, sc.save()
		}
	}
	return nil, fmt.Errorf("no such task")
}

// record applies a task's run bookkeeping. It deliberately skips the clock
// re-validation Update does: a time hand-edited into garbage in tasks.json would
// otherwise make every run fail to record, so LastRun never moves, LastErr never
// reaches the UI, and a one-shot never disables itself.
func (sc *Scheduler) record(id string, fn func(*Task)) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, t := range sc.tasks {
		if t.ID == id {
			fn(t)
			return sc.save()
		}
	}
	return fmt.Errorf("no such task")
}

func (sc *Scheduler) Delete(id string) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for i, t := range sc.tasks {
		if t.ID == id {
			sc.tasks = append(sc.tasks[:i], sc.tasks[i+1:]...)
			return sc.save()
		}
	}
	return fmt.Errorf("no such task")
}

// DropServer removes every task belonging to a server, and reports how many.
//
// Deleting a server used to leave its tasks in tasks.json forever. They were
// not merely untidy: the scheduler kept firing them on their schedule, each one
// reaching Run, failing "no such server" and recording that failure on a task
// nothing in the UI can show - because every task screen is reached through a
// server, and that server is gone. So the panel accumulated invisible work that
// ran and failed on a timer, and the only way to see it was to read the file.
//
// Worse on a busy panel: server IDs are minted from a clock, and a future
// server can be handed the ID a deleted one had. The orphaned task then finds a
// server again - a different server - and runs "stop; backup" against it on the
// old one's schedule.
func (sc *Scheduler) DropServer(serverID string) int {
	if serverID == "" {
		return 0
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	kept := sc.tasks[:0]
	dropped := 0
	for _, t := range sc.tasks {
		if t.ServerID == serverID {
			dropped++
			continue
		}
		kept = append(kept, t)
	}
	if dropped == 0 {
		return 0
	}
	sc.tasks = kept
	if err := sc.save(); err != nil {
		log.Printf("dropped %d task(s) for deleted server %s but could not save: %v", dropped, serverID, err)
	}
	return dropped
}

func (sc *Scheduler) Get(id string) *Task {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	for _, t := range sc.tasks {
		if t.ID == id {
			cp := *t
			return &cp
		}
	}
	return nil
}

// NextRun reports when a task will next fire, for the UI.
func (t *Task) NextRun(now time.Time) time.Time {
	secs, err := parseClock(t.Time)
	if err != nil {
		return time.Time{}
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	next := midnight.Add(time.Duration(secs) * time.Second)
	if !next.After(now) {
		if !t.Repeat {
			return time.Time{} // one-shot whose moment has passed today
		}
		next = next.Add(24 * time.Hour)
	}
	return next
}

// loop fires due tasks. It ticks every 20s and fires anything whose time has
// passed and that has not already run in this same minute, so a restarted panel
// does not re-fire a task it already ran, and a slow tick cannot skip one.
func (sc *Scheduler) loop() {
	defer recoverPanic("scheduler loop")
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for now := range t.C {
		secsNow := now.Hour()*3600 + now.Minute()*60 + now.Second()

		sc.mu.RLock()
		due := []*Task{}
		for _, task := range sc.tasks {
			if !task.Enabled {
				continue
			}
			at, err := parseClock(task.Time)
			if err != nil {
				continue
			}
			// Fire within a 60s window after the scheduled second, so a 20s
			// tick cannot miss it, and never twice for the same occurrence.
			if secsNow < at || secsNow > at+59 {
				continue
			}
			last := time.Unix(task.LastRun, 0)
			if task.LastRun > 0 && now.Sub(last) < 2*time.Minute {
				continue
			}
			due = append(due, task)
		}
		sc.mu.RUnlock()

		for _, task := range due {
			go func(id string) {
				defer recoverPanic("scheduled task " + id)
				_ = sc.Run(id, "scheduler")
			}(task.ID)
		}
	}
}

// Run executes a task now. Also used by the "Run now" button, which is the only
// honest way to let someone test a nightly restart without waiting for night.
func (sc *Scheduler) Run(id, actor string) error {
	// The loop decides a task is due before LastRun is written back at the end of
	// the run, so "Run now" landing in that window would execute a second copy:
	// two lifecycle calls on one server, or two interleaved
	// `say ...; !wait 60; !restart` streams restarting it twice.
	sc.runMu.Lock()
	if sc.running[id] {
		sc.runMu.Unlock()
		return fmt.Errorf("that task is already running")
	}
	sc.running[id] = true
	sc.runMu.Unlock()
	defer func() {
		sc.runMu.Lock()
		delete(sc.running, id)
		sc.runMu.Unlock()
	}()

	task := sc.Get(id)
	if task == nil {
		return fmt.Errorf("no such task")
	}
	s := sc.mgr.Get(task.ServerID)
	if s == nil {
		return fmt.Errorf("no such server")
	}

	var runErr error
	for _, step := range strings.Split(task.Commands, ";") {
		step = strings.TrimSpace(step)
		if step == "" {
			continue
		}
		if err := sc.step(s, step, actor); err != nil {
			runErr = fmt.Errorf("%q: %w", step, err)
			break
		}
	}

	now := time.Now()
	if err := sc.record(id, func(t *Task) {
		t.LastRun = now.Unix()
		t.Runs++
		if runErr != nil {
			t.LastErr = runErr.Error()
		} else {
			t.LastErr = ""
		}
		if !t.Repeat {
			t.Enabled = false // one-shot
		}
	}); err != nil {
		// Discarding this hides a task that ran but looks like it never did: the
		// loop sees the old LastRun and fires it again on the next tick, which
		// turns a nightly !restart into a repeating one.
		log.Printf("scheduler: task %s ran but the result could not be recorded: %v", id, err)
	}

	detail := task.Name
	if runErr != nil {
		detail += " — " + runErr.Error()
	}
	sc.mgr.audit(actor, "task.run", task.ServerID, detail)
	sc.mgr.broadcastEvent("task.run", task.ServerID)
	return runErr
}

// step runs one element of a task. `!` marks a panel action; anything else goes
// to the game's console.
func (sc *Scheduler) step(s *Server, step, actor string) error {
	if !strings.HasPrefix(step, "!") {
		if s.State() != StatusRunning {
			return fmt.Errorf("server is not running")
		}
		return sc.mgr.Send(s.ID, step, "command", actor)
	}

	fields := strings.Fields(strings.TrimPrefix(step, "!"))
	if len(fields) == 0 {
		return fmt.Errorf("empty action")
	}
	switch fields[0] {
	case "restart":
		return sc.mgr.Restart(s.ID)
	case "stop":
		return sc.mgr.Stop(s.ID)
	case "start":
		return sc.mgr.Start(s.ID)
	case "backup":
		note := "scheduled"
		if len(fields) > 1 {
			note = strings.Join(fields[1:], " ")
		}
		_, err := sc.mgr.CreateBackup(s, note, actor)
		return err
	case "wait":
		d := 5
		if len(fields) > 1 {
			d = atoi(fields[1])
		}
		if d < 0 || d > 900 {
			return fmt.Errorf("wait must be between 0 and 900 seconds")
		}
		time.Sleep(time.Duration(d) * time.Second)
		return nil
	}
	return fmt.Errorf("unknown action !%s (try restart, stop, start, backup, wait)", fields[0])
}

// TaskView adds the computed next-run time for the UI.
func (sc *Scheduler) TaskView(serverID string) []map[string]any {
	now := time.Now()
	tasks := sc.List(serverID)
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		var next int64
		if n := t.NextRun(now); !n.IsZero() && t.Enabled {
			next = n.Unix()
		}
		out = append(out, map[string]any{
			"id": t.ID, "server_id": t.ServerID, "name": t.Name,
			"commands": t.Commands, "time": t.Time, "repeat": t.Repeat,
			"enabled": t.Enabled, "last_run": t.LastRun, "last_err": t.LastErr,
			"runs": t.Runs, "next_run": next,
		})
	}
	return out
}
