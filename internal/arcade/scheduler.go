package arcade

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
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

	// Separate from mu because a run holds it for the whole task — minutes, if
	// the task waits — while mu is taken and released repeatedly underneath.
	runMu   sync.Mutex
	running map[string]bool

	// slots bounds how many distinct tasks may execute at once. The loop
	// starts one goroutine per due task, and nothing capped how many could
	// pile up - a panel restarted into a backlog of due tasks launched them
	// all simultaneously against servers sharing one host.
	slots chan struct{}
}

// maxConcurrentTaskRuns is deliberately small: scheduled work is restarts,
// backups and command sequences, all of which contend for the same disks and
// CPUs the games do.
const maxConcurrentTaskRuns = 4

func newScheduler(dataDir string, m *Manager) *Scheduler {
	s := &Scheduler{path: filepath.Join(dataDir, "tasks.json"), mgr: m, running: map[string]bool{},
		slots: make(chan struct{}, maxConcurrentTaskRuns)}
	if b, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(b, &s.tasks); err != nil {
			quarantine(s.path, err) // losing every task silently is worse
		}
		// A `[null]` element decodes successfully and panics the loop the
		// first time it dereferences the record; so does a task whose clock
		// is garbage (fired at midnight every night instead of being refused,
		// per the clockField note below). Invalid records are dropped loudly
		// rather than taking the scheduler down.
		kept := s.tasks[:0]
		for i, t := range s.tasks {
			if t == nil {
				log.Printf("scheduler: dropped a null task record at index %d", i)
				continue
			}
			if err := validateTask(t); err != nil {
				log.Printf("scheduler: dropped task %q (%s): %v", t.Name, t.ID, err)
				continue
			}
			kept = append(kept, t)
		}
		s.tasks = kept
	}
	return s
}

// save persists the current task list. Retained for DropServer; Add, Update,
// Delete and record build a prospective slice and publish only after the
// write commits, so a failed save cannot leave memory and disk disagreeing
// about which tasks exist.
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

// validateTask is the single rule set every write path enforces, so Add and
// Update cannot drift apart again.
func validateTask(t *Task) error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("a task name is required")
	}
	if strings.TrimSpace(t.Commands) == "" {
		return fmt.Errorf("at least one command is required")
	}
	_, err := parseClock(t.Time)
	return err
}

// Add validates and stores a new task built from the caller's fields.
//
// The caller's pointer is copied, never stored: the HTTP handler used to
// decode the request body straight into a Task, so a client could seed
// Runs/LastRun/LastErr bookkeeping, and the returned pointer stayed live
// while the scheduler mutated it. IDs are random and checked for uniqueness -
// the old `unix%100000 + seq%100` collided after 100 tasks in the same
// second, and again after every restart.
func (sc *Scheduler) Add(t *Task) (*Task, error) {
	if err := validateTask(t); err != nil {
		return nil, err
	}
	if sc.mgr.Get(t.ServerID) == nil {
		return nil, fmt.Errorf("no such server")
	}

	created := *t
	created.Runs, created.LastRun, created.LastErr = 0, 0, ""

	sc.mu.Lock()
	defer sc.mu.Unlock()
	newID, err := func() (string, error) {
		for attempt := 0; attempt < 20; attempt++ {
			suffix, err := randomHex(8)
			if err != nil {
				return "", err
			}
			id := "t" + suffix
			if !sc.idTakenLocked(id) {
				return id, nil
			}
		}
		return "", fmt.Errorf("could not allocate a unique task id")
	}()
	if err != nil {
		return nil, err
	}
	created.ID = newID
	// Persist the prospective list before publishing it: a failed save used
	// to leave the task scheduled in memory while the caller was told it was
	// rejected - an "uncreated" task that ran anyway, until restart.
	next := make([]*Task, 0, len(sc.tasks)+1)
	for _, existing := range sc.tasks {
		next = append(next, existing)
	}
	next = append(next, &created)
	if err := sc.persist(next); err != nil {
		return nil, err
	}
	sc.tasks = next
	cp := created
	return &cp, nil
}

// idTakenLocked reports whether a task ID is in use. Callers hold sc.mu.
func (sc *Scheduler) idTakenLocked(id string) bool {
	for _, t := range sc.tasks {
		if t.ID == id {
			return true
		}
	}
	return false
}

// persist writes a prospective task list atomically. Callers hold sc.mu and
// publish sc.tasks = next only when this returns nil.
func (sc *Scheduler) persist(next []*Task) error {
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(sc.path, b, 0o644)
}

// Update applies fn to the task identified by (serverID, id). Both must match:
// the HTTP routes carry a server ID in their URL, and the old lookup ignored
// it - a stale or mistaken URL edited a different server's task and attributed
// the change to the wrong server in the audit log.
func (sc *Scheduler) Update(serverID, id string, fn func(*Task)) (*Task, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, t := range sc.tasks {
		if t.ID != id || t.ServerID != serverID {
			continue
		}
		before := *t
		fn(t)
		if err := validateTask(t); err != nil {
			// A rejected edit must not survive in memory. Leaving a bad
			// task on the live schedule would keep firing a task the
			// operator was just told was left unchanged - and the pointer
			// PATCH fix made blank names/commands reachable, which used
			// to record successful runs of nothing.
			*t = before
			return nil, err
		}
		next := make([]*Task, 0, len(sc.tasks))
		for _, existing := range sc.tasks {
			next = append(next, existing)
		}
		if err := sc.persist(next); err != nil {
			*t = before
			return nil, err
		}
		cp := *t
		return &cp, nil
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
			next := make([]*Task, 0, len(sc.tasks))
			for _, existing := range sc.tasks {
				next = append(next, existing)
			}
			return sc.persist(next)
		}
	}
	return fmt.Errorf("no such task")
}

// Delete removes the task identified by (serverID, id); both must match, for
// the same reason Update checks both.
func (sc *Scheduler) Delete(serverID, id string) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for i, t := range sc.tasks {
		if t.ID != id || t.ServerID != serverID {
			continue
		}
		next := make([]*Task, 0, len(sc.tasks)-1)
		next = append(next, sc.tasks[:i]...)
		next = append(next, sc.tasks[i+1:]...)
		if err := sc.persist(next); err != nil {
			return err
		}
		sc.tasks = next
		return nil
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
//
// Built with time.Date on the civil fields, not midnight-plus-duration: on a
// daylight-saving day the clock jumps, and midnight+3h30 landed on 04:30 when
// the task says 03:30 - the preview disagreed with the loop, which compares
// wall-clock time, on exactly the days people schedule restarts around.
func (t *Task) NextRun(now time.Time) time.Time {
	secs, err := parseClock(t.Time)
	if err != nil {
		return time.Time{}
	}
	h, m, s := secs/3600, (secs%3600)/60, secs%60
	loc := now.Location()
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, s, 0, loc)
	if !next.After(now) {
		if !t.Repeat {
			return time.Time{} // one-shot whose moment has passed today
		}
		tomorrow := now.AddDate(0, 0, 1)
		next = time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), h, m, s, 0, loc)
	}
	return next
}

// sameLocalDate reports whether two moments fall on the same local calendar
// day. This is the occurrence identity the loop suppresses repeats with: on
// the fall-back day the same local hour happens twice, and a "not run in the
// last two minutes" check let a 01:30 task fire in both of them.
func sameLocalDate(a, b time.Time) bool {
	a, b = a.Local(), b.Local()
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// loop fires due tasks. It ticks every 20s and fires anything whose time has
// passed and that has not already run on this local date, so a restarted panel
// does not re-fire a task it already ran, a slow tick cannot skip one, and a
// repeated hour at a daylight transition does not double-fire one.
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
			if task.LastRun > 0 && sameLocalDate(time.Unix(task.LastRun, 0), now) {
				continue
			}
			due = append(due, task)
		}
		sc.mu.RUnlock()

		for _, task := range due {
			// Admission before the goroutine: without it a panel restarting
			// into a pile of due tasks spawned one goroutine per task, all
			// waiting (and then all running) at once.
			select {
			case sc.slots <- struct{}{}:
			default:
				log.Printf("scheduler: %d task runs are already executing; skipping %q this occurrence",
					maxConcurrentTaskRuns, task.Name)
				continue
			}
			go func(id string) {
				defer recoverPanic("scheduled task " + id)
				defer func() { <-sc.slots }()
				_ = sc.Run("", id, "scheduler")
			}(task.ID)
		}
	}
}

// Run executes a task now. Also used by the "Run now" button, which is the only
// honest way to let someone test a nightly restart without waiting for night.
// serverID may be empty to mean "the task's own server"; routes that carry a
// server ID in their URL pass it so a mismatched URL cannot drive another
// server's task.
func (sc *Scheduler) Run(serverID, id, actor string) error {
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
	if serverID != "" && task.ServerID != serverID {
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
			// Strict, not atoi: atoi's contract returns 0 for anything it
			// cannot parse, 0 is a valid wait, and "!wait sixty" in a
			// restart sequence restarted immediately after announcing a
			// delay. parseClock got a strict parser for exactly this class.
			var err error
			d, err = strconv.Atoi(fields[1])
			if err != nil {
				return fmt.Errorf("wait must be a whole number of seconds")
			}
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
