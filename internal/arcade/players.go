package arcade

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
