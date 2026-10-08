package arcade

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
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
	extra   map[string]json.RawMessage
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
	root, err := os.OpenRoot(m.serverDir(s))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, st, err := openRegularIn(root, name)
	if os.IsNotExist(err) {
		return []ListEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st.Size() > maxBodyBytes {
		return nil, fmt.Errorf("player list exceeds size limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBodyBytes {
		return nil, fmt.Errorf("player list exceeds size limit")
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
	// A shared mutation of the server tree like any other: held on the
	// filesystem gate so a list edit cannot land inside a backup/restore
	// window. Callers already inside an fsMu read section use
	// writeListHeld - taking RLock recursively here deadlocked against
	// Start's fsMu -> lifecycle order.
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()
	s.editMu.Lock()
	defer s.editMu.Unlock()
	return m.writeListHeld(s, l, entries)
}

func (m *Manager) writeListHeld(s *Server, l PlayerList, entries []ListEntry) error {
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
	// fsMu BEFORE lifecycle - the only legal order. The previous shape took
	// lifecycle first and reached fsMu through writeList, while Start takes
	// fsMu first and waits for lifecycle: one list edit plus one start of
	// the same server deadlocked the whole process (lifecycle is global).
	s.fsMu.RLock()
	defer s.fsMu.RUnlock()
	s.editMu.Lock()
	defer s.editMu.Unlock()

	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if err := m.requireRegistered(s); err != nil {
		return false, err
	}

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
	if l == ListBannedIPs {
		ip, err := netip.ParseAddr(who)
		if err != nil {
			return fmt.Errorf("a valid IPv4 or IPv6 address is required")
		}
		who = ip.Unmap().String()
	}
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
		return m.applyListAddHeld(s, l, who, reason)
	})
	if err != nil {
		return err
	}
	if viaConsole {
		if err := m.Send(s.ID, gameCommand(l, true, who, reason), "command", actor); err != nil {
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
	s.editMu.Lock()
	defer s.editMu.Unlock()
	return m.applyListAddHeld(s, l, who, reason)
}
func (m *Manager) applyListAddHeld(s *Server, l PlayerList, who, reason string) error {
	entries, err := m.readList(s, l)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if playerListKeyEqual(l, e.Key(), who) {
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
		ip, err := netip.ParseAddr(who)
		if err != nil {
			return fmt.Errorf("a valid IPv4 or IPv6 address is required")
		}
		e.IP = ip.Unmap().String()
	} else {
		e.Name = who
		uuid, err := m.playerListUUID(s, who)
		if err != nil {
			return err
		}
		e.UUID = uuid
		if l == ListOps {
			e.Level = 4
		}
	}
	if e.Reason == "" && (l == ListBanned || l == ListBannedIPs) {
		e.Reason = "Banned by an operator"
	}
	return m.writeListHeld(s, l, append(entries, e))
}

func (m *Manager) RemoveFromList(s *Server, l PlayerList, who, actor string) error {
	who = strings.TrimSpace(who)
	if who == "" || strings.Contains(who, " ") {
		return fmt.Errorf("a valid name or IP is required")
	}
	if l == ListBannedIPs {
		ip, err := netip.ParseAddr(who)
		if err != nil {
			return fmt.Errorf("a valid IPv4 or IPv6 address is required")
		}
		who = ip.Unmap().String()
	}
	// The remove path composes a console command too, so "x\nop attacker" is the
	// same escalation as a poisoned ban reason.
	if hasControl(who) {
		return fmt.Errorf("%q is not a valid name", who)
	}

	viaConsole, err := m.routeListChange(s, l, func() error {
		return m.applyListRemoveHeld(s, l, who)
	})
	if err != nil {
		return err
	}
	if viaConsole {
		if err := m.Send(s.ID, gameCommand(l, false, who, ""), "command", actor); err != nil {
			return err
		}
	}
	m.audit(actor, "players.remove", s.ID, string(l)+": "+who)
	return nil
}

func (m *Manager) applyListRemove(s *Server, l PlayerList, who string) error {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	return m.applyListRemoveHeld(s, l, who)
}
func (m *Manager) applyListRemoveHeld(s *Server, l PlayerList, who string) error {
	entries, err := m.readList(s, l)
	if err != nil {
		return err
	}
	out := entries[:0]
	found := false
	for _, e := range entries {
		if playerListKeyEqual(l, e.Key(), who) {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		return fmt.Errorf("%s is not on the %s", who, l.label())
	}
	return m.writeListHeld(s, l, out)
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
		next = append(next, Player{Name: n, Source: "rcon", JoinedAt: time.Now().Unix()})
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

// playerSyncInterval is how often the panel re-asks a game who is actually on.
//
// Five minutes, not one. The reconcile is a safety net now rather than the
// primary source: joins and departures are read from `logged in with entity id`
// and `lost connection:`, which the server writes itself and no plugin can
// cancel. What is left for the net to catch is a line shed under backpressure
// or one that fell outside a restart's replay window, and neither needs asking
// every minute. Each ask costs an RCON connect, a command and a disconnect in
// the operator's own log.
const playerSyncInterval = 5 * time.Minute

// playerSyncLoop keeps the sidebar honest for the consoles somebody is watching.
//
// Console parsing is the fast path and the only one a proxy has, but it is a
// stream of announcements, and an announcement can be missed: a plugin can
// cancel the join broadcast, the socket can shed lines under backpressure, and
// a panel restart replays only the tail. Every one of those leaves the list
// quietly wrong, with nothing to indicate it - which is the failure an operator
// reports as "the panel does not see my players".
//
// Gated on there being a viewer, which the first version was not, and that was
// a mistake with a visible cost. Every RCON query opens a connection, runs the
// command and closes, and the game logs all three - so a panel nobody was
// looking at wrote this into every Paper server's log, once a minute, forever:
//
//	Thread RCON Client /0:0:0:0:0:0:0:1 started
//	[Essentials] Rcon issued server command: /list
//	Thread RCON Client /0:0:0:0:0:0:0:1 shutting down
//
// Three lines a minute per server, in the operator's own log file, describing
// nothing they did. The player list only matters while somebody is reading it,
// so that is when it is worth asking - an idle panel now produces no RCON
// traffic at all, and a watched console is corrected exactly as before.
func (m *Manager) playerSyncLoop() {
	defer recoverPanic("player sync")
	t := time.NewTicker(playerSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
		for _, s := range m.List() {
			if m.hub.Viewers(s.ID) == 0 {
				continue
			}
			m.reconcilePlayers(s)
		}
	}
}

func (e *ListEntry) UnmarshalJSON(b []byte) error {
	type fields ListEntry
	var value fields
	if err := json.Unmarshal(b, &value); err != nil {
		return err
	}
	if err := json.Unmarshal(b, &value.extra); err != nil {
		return err
	}
	for _, key := range []string{"name", "uuid", "ip", "level", "reason", "source", "created", "expires"} {
		delete(value.extra, key)
	}
	*e = ListEntry(value)
	return nil
}
func (e ListEntry) MarshalJSON() ([]byte, error) {
	type fields ListEntry
	b, err := json.Marshal(fields(e))
	if err != nil {
		return nil, err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(b, &values); err != nil {
		return nil, err
	}
	for key, value := range e.extra {
		values[key] = value
	}
	return json.Marshal(values)
}

// Stopped edits never invent an online identity. Local cache/list records are
// the only permitted evidence; conflicts or unknown identities require the
// game to resolve them while running. Offline UUIDs exactly match Java's
// UUID.nameUUIDFromBytes(UTF8("OfflinePlayer:" + case-sensitive name)).
func offlinePlayerUUID(name string) string {
	sum := md5.Sum([]byte("OfflinePlayer:" + name)) // Java's required identity format, not a credential hash.
	sum[6] = (sum[6] & 0x0f) | 0x30
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
func canonicalPlayerUUID(raw string) (string, error) {
	if len(raw) != 36 || raw[8] != '-' || raw[13] != '-' || raw[18] != '-' || raw[23] != '-' {
		return "", fmt.Errorf("player UUID must use canonical 8-4-4-4-12 form")
	}
	bytes, err := hex.DecodeString(strings.ReplaceAll(raw, "-", ""))
	if err != nil || len(bytes) != 16 {
		return "", fmt.Errorf("invalid player UUID")
	}
	zero := true
	for _, b := range bytes {
		if b != 0 {
			zero = false
		}
	}
	if zero {
		return "", fmt.Errorf("nil player UUID is not an identity")
	}
	return strings.ToLower(raw), nil
}

const maxIdentityCacheBytes = 1 << 20

func (m *Manager) playerListUUID(s *Server, who string) (string, error) {
	if s.Runtime == RuntimeSim && s.State() == StatusRunning {
		return fakeUUID(who), nil
	} // simulator owns resolution of its live commands
	switch strings.ToLower(strings.TrimSpace(s.Prop("online-mode"))) {
	case "false":
		return offlinePlayerUUID(who), nil
	case "", "true":
	default:
		return "", fmt.Errorf("online-mode must be true or false before editing player identities")
	}
	found := ""
	consider := func(name, raw string) error {
		if !strings.EqualFold(name, who) {
			return nil
		}
		uuid, err := canonicalPlayerUUID(raw)
		if err != nil {
			return fmt.Errorf("known record for %s has an invalid UUID; repair it before editing lists", who)
		}
		if found != "" && found != uuid {
			return fmt.Errorf("conflicting known UUIDs for %s; repair local identity records", who)
		}
		found = uuid
		return nil
	}
	root, err := os.OpenRoot(m.serverDir(s))
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, info, err := openRegularIn(root, "usercache.json")
	if err == nil {
		if info.Size() > maxIdentityCacheBytes {
			f.Close()
			return "", fmt.Errorf("usercache.json exceeds identity lookup limit")
		}
		body, readErr := io.ReadAll(io.LimitReader(f, maxIdentityCacheBytes+1))
		closeErr := f.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return "", err
		}
		if len(body) > maxIdentityCacheBytes {
			return "", fmt.Errorf("usercache.json exceeds identity lookup limit")
		}
		var profiles []*struct {
			Name string `json:"name"`
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(body, &profiles) != nil || profiles == nil || len(profiles) > 10000 {
			return "", fmt.Errorf("usercache.json is not a bounded profile array")
		}
		for _, profile := range profiles {
			if profile == nil {
				return "", fmt.Errorf("usercache.json contains a null profile")
			}
			if err := consider(profile.Name, profile.UUID); err != nil {
				return "", err
			}
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	for _, list := range []PlayerList{ListWhitelist, ListOps, ListBanned} {
		entries, err := m.readList(s, list)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if err := consider(entry.Name, entry.UUID); err != nil {
				return "", err
			}
		}
	}
	if found == "" {
		return "", fmt.Errorf("online identity for %s is unknown; start the server so the game can resolve it, or provide a known matching profile in usercache.json", who)
	}
	return found, nil
}

func playerListKeyEqual(list PlayerList, a, b string) bool {
	if list == ListBannedIPs {
		ipA, errA := netip.ParseAddr(a)
		ipB, errB := netip.ParseAddr(b)
		return errA == nil && errB == nil && ipA.Unmap() == ipB.Unmap()
	}
	return strings.EqualFold(a, b)
}
