package arcade

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Player management: whitelist, operators, banned players, banned IPs.
//
// These are the game's own JSON files. The rule that keeps the panel and the
// game from fighting over them:
//
//	server running -> issue the game command; the game rewrites its own file
//	server stopped -> edit the file directly
//
// Doing it the other way round (writing the file under a running server) means
// the game overwrites the edit on its next save, and the change silently
// vanishes — which is exactly the kind of bug an operator never reports because
// they assume they clicked wrong.

type PlayerList string

const (
	ListWhitelist PlayerList = "whitelist"
	ListOps       PlayerList = "ops"
	ListBanned    PlayerList = "banned"
	ListBannedIPs PlayerList = "banned-ips"
)

func (l PlayerList) file() (string, error) {
	switch l {
	case ListWhitelist:
		return "whitelist.json", nil
	case ListOps:
		return "ops.json", nil
	case ListBanned:
		return "banned-players.json", nil
	case ListBannedIPs:
		return "banned-ips.json", nil
	}
	return "", fmt.Errorf("unknown list %q", l)
}

func (l PlayerList) label() string {
	switch l {
	case ListWhitelist:
		return "Whitelist"
	case ListOps:
		return "Operators"
	case ListBanned:
		return "Banned"
	case ListBannedIPs:
		return "Banned IPs"
	}
	return string(l)
}

// ListEntry is the union of the shapes the four files use. Minecraft's own
// files disagree with each other, so we normalise on read.
type ListEntry struct {
	Name    string `json:"name,omitempty"`
	UUID    string `json:"uuid,omitempty"`
	IP      string `json:"ip,omitempty"`
	Level   int    `json:"level,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Source  string `json:"source,omitempty"`
	Created string `json:"created,omitempty"`
	Expires string `json:"expires,omitempty"`
}

// Key is what identifies the entry to the user and to a remove command.
func (e ListEntry) Key() string {
	if e.IP != "" {
		return e.IP
	}
	return e.Name
}

func (m *Manager) readList(s *Server, l PlayerList) ([]ListEntry, error) {
	name, err := l.file()
	if err != nil {
		return nil, err
	}
	dir, err := m.ensureServerDir(s)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return []ListEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ListEntry
	if err := json.Unmarshal(b, &out); err != nil {
		// A hand-edited file that no longer parses should say so rather than
		// silently presenting an empty list.
		return nil, fmt.Errorf("%s is not valid JSON: %w", name, err)
	}
	if out == nil {
		out = []ListEntry{}
	}
	return out, nil
}

func (m *Manager) writeList(s *Server, l PlayerList, entries []ListEntry) error {
	name, err := l.file()
	if err != nil {
		return err
	}
	dir, err := m.ensureServerDir(s)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, name), append(b, '\n'), 0o644)
}

// hasControl reports whether s carries a character that would break out of the
// single line a console command is composed onto. Newline is the dangerous one:
// a game console reads one command per line, so a value containing "\n" smuggles
// a second command in behind the first — a ban reason of "cheating\nop attacker"
// bans the griefer and then hands out operator.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// gameCommand maps a list change onto the command the running game understands.
// Both who and reason land verbatim on the console line, so callers must reject
// anything hasControl flags before composing.
func gameCommand(l PlayerList, add bool, who, reason string) string {
	switch l {
	case ListWhitelist:
		if add {
			return "whitelist add " + who
		}
		return "whitelist remove " + who
	case ListOps:
		if add {
			return "op " + who
		}
		return "deop " + who
	case ListBanned:
		if add {
			if reason != "" {
				return "ban " + who + " " + reason
			}
			return "ban " + who
		}
		return "pardon " + who
	case ListBannedIPs:
		if add {
			if reason != "" {
				return "ban-ip " + who + " " + reason
			}
			return "ban-ip " + who
		}
		return "pardon-ip " + who
	}
	return ""
}

// routeListChange picks the safe route for a list change and, when that route is
// the file, performs the edit before letting go of the lock.
//
// The state check and the write have to be one step. Manager.Start takes this
// same lock and is the only path that hands these files to a game process, so
// while it is held a stopped server cannot begin running in the gap - and
// without that the game boots on the pre-edit file and rewrites it on its next
// save, which is the silent loss the split at the top of this file exists to
// prevent, reached from the other side.
//
// Starting and stopping are refused rather than written for the same reason: in
// both the game already owns these files, so an operator asked to wait a few
// seconds comes off far better than one whose change disappears with nothing to
// report.
func (m *Manager) routeListChange(s *Server, l PlayerList, edit func() error) (viaConsole bool, err error) {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()

	switch st := s.State(); st {
	case StatusRunning:
		// The send itself is left to the caller, outside this lock: an adopted
		// container takes commands over `docker exec rcon-cli`, which has no
		// timeout, and Start and Delete must not queue behind it.
		return true, nil
	case StatusStopped, StatusFailed:
		return false, edit()
	default:
		return false, fmt.Errorf("the server is %s; wait until it has settled before changing the %s",
			st, l.label())
	}
}

func (m *Manager) AddToList(s *Server, l PlayerList, who, reason, actor string) error {
	who = strings.TrimSpace(who)
	if who == "" {
		return fmt.Errorf("a name or IP is required")
	}
	if strings.Contains(who, " ") || hasControl(who) {
		return fmt.Errorf("%q is not a valid name", who)
	}
	// The reason is trusted nowhere else and reaches the console verbatim; an
	// operator is trusted with ban, not with whatever command follows a newline.
	if hasControl(reason) {
		return fmt.Errorf("the reason cannot contain line breaks or control characters")
	}

	viaConsole, err := m.routeListChange(s, l, func() error {
		return m.applyListAdd(s, l, who, reason)
	})
	if err != nil {
		return err
	}
	if viaConsole {
		if err := m.runnerFor(s).Send(s, gameCommand(l, true, who, reason)); err != nil {
			return err
		}
	}
	m.audit(actor, "players.add", s.ID, string(l)+": "+who)
	return nil
}

// applyListAdd edits the file directly. Used by the stopped path, and by the
// simulator when it handles a whitelist/op/ban command - a simulator that
// acknowledges a command without applying it is worse than one that refuses.
func (m *Manager) applyListAdd(s *Server, l PlayerList, who, reason string) error {
	entries, err := m.readList(s, l)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Key(), who) {
			return fmt.Errorf("%s is already on the %s", who, l.label())
		}
	}
	e := ListEntry{
		Created: time.Now().Format("2006-01-02 15:04:05 -0700"),
		Source:  "Teploy Arcade",
		Expires: "forever",
		Reason:  reason,
	}
	if l == ListBannedIPs {
		e.IP = who
	} else {
		e.Name = who
		// The game fills the real UUID on first contact; offline placeholders
		// would be worse than an empty field.
		if l == ListOps {
			e.Level = 4
		}
	}
	if e.Reason == "" && (l == ListBanned || l == ListBannedIPs) {
		e.Reason = "Banned by an operator"
	}
	return m.writeList(s, l, append(entries, e))
}

func (m *Manager) RemoveFromList(s *Server, l PlayerList, who, actor string) error {
	// The remove path composes a console command too, so "x\nop attacker" is the
	// same escalation as a poisoned ban reason.
	if hasControl(who) {
		return fmt.Errorf("%q is not a valid name", who)
	}

	viaConsole, err := m.routeListChange(s, l, func() error {
		return m.applyListRemove(s, l, who)
	})
	if err != nil {
		return err
	}
	if viaConsole {
		if err := m.runnerFor(s).Send(s, gameCommand(l, false, who, "")); err != nil {
			return err
		}
	}
	m.audit(actor, "players.remove", s.ID, string(l)+": "+who)
	return nil
}

func (m *Manager) applyListRemove(s *Server, l PlayerList, who string) error {
	entries, err := m.readList(s, l)
	if err != nil {
		return err
	}
	out := entries[:0]
	found := false
	for _, e := range entries {
		if strings.EqualFold(e.Key(), who) {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		return fmt.Errorf("%s is not on the %s", who, l.label())
	}
	return m.writeList(s, l, out)
}

// PlayerLists returns all four lists plus who is online, which is what the
// Players screen renders in one payload.
func (m *Manager) PlayerLists(s *Server) (map[string]any, error) {
	out := map[string]any{}
	for _, l := range []PlayerList{ListWhitelist, ListOps, ListBanned, ListBannedIPs} {
		entries, err := m.readList(s, l)
		if err != nil {
			// One unparseable file must not blank the whole screen.
			out[string(l)] = []ListEntry{}
			out[string(l)+"_error"] = err.Error()
			continue
		}
		out[string(l)] = entries
	}

	s.mu.Lock()
	online := make([]Player, len(s.players))
	copy(online, s.players)
	// Read under the same lock rather than off s.Props directly: reloadProps
	// writes that map whenever server.properties is edited, and Go aborts the
	// whole process on a concurrent map read/write - it is not a panic anything
	// can recover.
	enforced := s.Props["white-list"] == "true"
	s.mu.Unlock()

	out["online"] = online
	out["running"] = s.State() == StatusRunning
	out["whitelist_enforced"] = enforced
	return out, nil
}

// ------------------------------------------------------- who is actually on

// parsePlayerList reads the game's own answer to `list`.
//
// There is no single format. The deployed fleet answers, with ANSI colour
// codes around every number:
//
//	There are 0 out of maximum 20 players online.
//
// vanilla answers:
//
//	There are 2 of a max of 20 players online: Alice, Bob
//
// and a plugin can replace either - EssentialsX is doing exactly that on this
// fleet, which is where "There's no one online in this group!" comes from. The
// first version of this function accepted only the vanilla sentence, so on the
// one host it was written for it silently matched nothing and the reconcile did
// nothing at all. Silent failure plus a wrong format is a feature that ships
// dead, which is why the strings below are copied from that host.
//
// So: take the count from either sentence, take names only when the reply
// actually lists them, and refuse everything else. ok=false means "keep what
// the console told you", which is always better than replacing a real list with
// a guess.
func parsePlayerList(out string) ([]string, bool) {
	clean := ansiRe.ReplaceAllString(out, "")

	loc := listCountRe.FindStringSubmatchIndex(clean)
	if loc == nil {
		return nil, false
	}
	count := atoi(clean[loc[2]:loc[3]])

	// "0 players online." is a complete answer on its own: nobody is on, and
	// whatever a plugin prints after it cannot change that.
	if count == 0 {
		return []string{}, true
	}

	// Names follow a colon - but not necessarily on the count's own line.
	// Vanilla answers on one line:
	//
	//	There are 2 of a max of 20 players online: Alice, Bob
	//
	// EssentialsX, which this fleet runs, answers the count and then one line
	// per permission group:
	//
	//	There are 1 out of maximum 20 players online.
	//	default: Steve_Example
	//
	// The first version confined the search to the count's line, to stop
	// EssentialsX's "Error: There's no one online in this group!" from yielding
	// a player called "There's". It worked - and it also refused every answer
	// that had anybody in it, on the one fleet it was written for. So the
	// reconcile did nothing whenever there was something to do.
	//
	// The count is what makes reading further lines safe. Names are collected
	// from the whole reply and accepted only when there are exactly as many as
	// the server itself said were online. A stray word from a decoration line
	// changes the total, and the whole answer is refused - which leaves the
	// console's own tracking in place. Never a wrong list, only ever no list.
	var names []string
	for _, line := range strings.Split(clean[loc[1]:], "\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		// "Error:" is a message about the answer, not part of it.
		if strings.EqualFold(strings.TrimSpace(line[:i]), "error") {
			continue
		}
		for _, part := range strings.Split(line[i+1:], ",") {
			if name := playerName(part); name != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) != count {
		return nil, false
	}
	return names, true
}

// playerName pulls a bare account name out of one decorated list entry.
//
// Plugins decorate names constantly - prefixes, colours, "(afk)" - so take the
// first bare word and keep it only if it could be a Minecraft name.
func playerName(part string) string {
	name := strings.TrimSpace(part)
	if k := strings.IndexAny(name, " \t"); k > 0 {
		name = name[:k]
	}
	name = strings.TrimFunc(name, func(r rune) bool {
		return !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	})
	if len(name) < 3 || len(name) > 16 {
		return ""
	}
	return name
}

var (
	ansiRe      = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	listCountRe = regexp.MustCompile(`There are (\d+) (?:of a max of|out of maximum) (\d+) players online`)
)

// reconcilePlayers replaces the in-memory player list with the game's own.
//
// Only meaningful for a runtime that can be asked. A failure is silent on
// purpose: the common case is an image without rcon-cli, where the panel simply
// keeps what the console told it.
func (m *Manager) reconcilePlayers(s *Server) {
	defer recoverPanic("reconcile players for " + s.ID)
	if !m.canAskWhoIsOnline(s) {
		return
	}
	dr, _ := m.runnerFor(s).(*dockerRunner)
	out, err := dr.query(s, "list")
	if err != nil {
		return
	}
	names, ok := parsePlayerList(out)
	if !ok {
		return
	}

	s.mu.Lock()
	prev := make(map[string]Player, len(s.players))
	for _, p := range s.players {
		prev[p.Name] = p
	}
	next := make([]Player, 0, len(names))
	changed := len(names) != len(s.players)
	for _, n := range names {
		// Keep the join time for anyone the console already knew about, so a
		// reconcile does not reset every session clock to the panel's restart.
		if p, ok := prev[n]; ok {
			next = append(next, p)
			continue
		}
		// Same count, different people is still a change - a swap during the
		// window the panel was not watching would otherwise go unlogged.
		changed = true
		next = append(next, Player{Name: n, UUID: fakeUUID(n), PingMS: 30, JoinedAt: time.Now().Unix()})
	}
	s.players = next
	s.mu.Unlock()

	if changed {
		log.Printf("%s: %d player(s) online according to the server itself", s.Name, len(next))
	}
	m.pushPlayers(s)
}

// canAskWhoIsOnline reports whether `list` over RCON is worth attempting.
//
// A proxy is excluded rather than left to fail: Velocity speaks no RCON at all,
// so the itzg/bungeecord image's rcon-cli answers every query with
// "authentication failed" - a `docker exec` per server per tick, forever, for
// an answer that can never arrive. The proxy's list comes from its own console
// instead, which is the one place a connect is always announced.
func (m *Manager) canAskWhoIsOnline(s *Server) bool {
	if _, ok := m.runnerFor(s).(*dockerRunner); !ok {
		return false
	}
	s.mu.Lock()
	game, status, image, mode := s.Game, s.Status, s.Image, consoleMode(s)
	s.mu.Unlock()
	return game != "proxy" && status == StatusRunning && mode == ConsoleRCON && hasRCON(image)
}

// playerSyncInterval is how often the panel re-asks every running game who is
// actually on. Long enough that it is not a load, short enough that a sidebar
// is never wrong for a whole session.
const playerSyncInterval = 60 * time.Second

// playerSyncLoop keeps the sidebar honest.
//
// Console parsing is the fast path and the only one a proxy has, but it is a
// stream of announcements, and an announcement can be missed: a plugin can
// cancel the join broadcast, the socket can shed lines under backpressure, and
// a panel restart replays only the tail. Every one of those leaves the list
// quietly wrong, with nothing to indicate it - which is the failure an operator
// reports as "the panel does not see my players".
//
// So the announcements stay, and once a minute the game itself gets the last
// word.
func (m *Manager) playerSyncLoop() {
	defer recoverPanic("player sync")
	t := time.NewTicker(playerSyncInterval)
	defer t.Stop()
	for range t.C {
		for _, s := range m.List() {
			m.reconcilePlayers(s)
		}
	}
}
