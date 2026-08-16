package arcade

import "testing"

// The proxy's Players sidebar sat empty through a whole session while players
// were demonstrably on the network.
//
// A proxy never runs a world, so nothing ever "joins the game" in its log, and
// joinRe is the only thing that fed the player list. Every player on the
// network connects to the proxy - it is the front door and the console an
// operator watches to see who is on - so the one server that should always
// know was the one that never did.
//
// The lines below are copied from the deployed Velocity's own log, with the
// player name replaced - the shapes are exact, the identity is not real.
func TestProxyConnectLinesTrackPlayers(t *testing.T) {
	cases := []struct {
		line    string
		want    string // player name, "" for no match
		joining bool
	}{
		{`[23:57:03 INFO]: [connected player] Steve_Example (/192.168.1.160:50545) has connected`, "Steve_Example", true},
		{`[01:02:14 INFO]: [connected player] Steve_Example (/192.168.1.160:50545) has disconnected`, "Steve_Example", false},

		// The same log carries the backend handoff. Counting it as a second
		// arrival double-counts one player, and counts them out again every
		// time they hop between backends - which on this network is the normal
		// way to move around.
		{`[23:57:03 INFO]: [server connection] Steve_Example -> Lobby has connected`, "", false},
		{`[01:02:14 INFO]: [server connection] Steve_Example -> Lobby has disconnected`, "", false},

		// Nothing else in a proxy log should look like a player.
		{`[23:57:03 INFO]: Booting up Velocity 3.3.0...`, "", false},
		{`[23:57:03 INFO]: Done (1.43s)!`, "", false},
	}

	for _, c := range cases {
		m := proxyJoinRe.FindStringSubmatch(c.line)
		if c.want == "" {
			if m != nil {
				t.Errorf("matched a line that is not a player session: %q -> %q", c.line, m[1])
			}
			continue
		}
		if m == nil {
			t.Errorf("no match on a real connect line: %q", c.line)
			continue
		}
		if m[1] != c.want {
			t.Errorf("player is %q, want %q, in %q", m[1], c.want, c.line)
		}
		if joined := m[2] == "connected"; joined != c.joining {
			t.Errorf("joining=%v, want %v, in %q", joined, c.joining, c.line)
		}
	}
}

// A backend's own join line must keep working exactly as before, and must not
// be picked up twice now that there are two patterns.
func TestBackendJoinLinesStillTrackPlayers(t *testing.T) {
	line := `[23:52:16 INFO]: Steve_Example joined the game`
	m := joinRe.FindStringSubmatch(line)
	if m == nil || m[1] != "Steve_Example" || m[2] != "joined" {
		t.Fatalf("backend join line no longer parses: %q -> %v", line, m)
	}
	if proxyJoinRe.MatchString(line) {
		t.Error("the proxy pattern also matches a backend join; that player would be counted twice")
	}
}

// trackPlayer is the shared sink both patterns feed, so a player who connects
// through the proxy and then leaves must not linger in the list.
func TestTrackPlayerAddsAndRemoves(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]

	mgr.trackPlayer(s, "Steve_Example", true)
	mgr.trackPlayer(s, "Steve_Example", true) // a reconnect must not duplicate

	// Deliberately not asserting on Snapshot()["players"] here: a stopped
	// server reports no player block at all, because "0 of 20" claims nobody is
	// playing, which is a different fact from "not running".
	s.mu.Lock()
	n := len(s.players)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("player list has %d entries after two joins by one player", n)
	}

	mgr.trackPlayer(s, "Steve_Example", false)
	s.mu.Lock()
	n = len(s.players)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("player list still has %d entries after the player left", n)
	}
}

// The Lobby on the deployed network never prints "joined the game" - a plugin
// cancels the broadcast - so the panel saw people leave a world it had never
// seen them enter, and the sidebar stayed empty for a player who was standing
// in it. These lines are copied from that server's own log, with the player
// name replaced.
func TestServerAuthoredLinesTrackPlayers(t *testing.T) {
	const login = `[03:18:06 INFO]: Steve_Example[/192.168.1.160:46714] logged in with entity id 9 at ([minecraft:overworld]8.46, 3.0, 2.94)`
	const lost = `[03:41:11 INFO]: Steve_Example lost connection: Disconnected`

	m := loginRe.FindStringSubmatch(login)
	if m == nil || m[1] != "Steve_Example" {
		t.Fatalf("a login the panel must see did not parse: %q -> %v", login, m)
	}
	m = lostConnRe.FindStringSubmatch(lost)
	if m == nil || m[1] != "Steve_Example" {
		t.Fatalf("a disconnect the panel must see did not parse: %q -> %v", lost, m)
	}

	// The timestamp prefix is the trap: "INFO]" is a word followed by a
	// bracket, and a looser pattern reads it as a player name.
	for _, line := range []string{
		`[03:17:58 INFO]: UUID of player Steve_Example is 00000000-0000-3000-8000-000000000000`,
		`[03:46:18 INFO]: [Metrics] Connection refused`,
		`[23:57:03 INFO]: Done (1.43s)!`,
	} {
		if loginRe.MatchString(line) || lostConnRe.MatchString(line) {
			t.Errorf("a line that is not a player session was read as one: %q", line)
		}
	}
}

// A server that prints both the broadcast and its own line announces one
// arrival twice. The sidebar must show one player.
func TestBroadcastAndServerLineCountOnePlayer(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]

	for _, line := range []string{
		`[03:18:06 INFO]: Steve_Example[/192.168.1.160:46714] logged in with entity id 9 at (x)`,
		`[03:18:06 INFO]: Steve_Example joined the game`,
	} {
		if m := joinRe.FindStringSubmatch(line); m != nil {
			mgr.trackPlayer(s, m[1], m[2] == "joined")
		} else if m := loginRe.FindStringSubmatch(line); m != nil {
			mgr.trackPlayer(s, m[1], true)
		}
	}
	s.mu.Lock()
	n := len(s.players)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("one arrival announced twice produced %d players", n)
	}
}

// A panel restart loses who is online: the tail replays the last 200 lines and
// rebuilds recent arrivals only, so a player who joined an hour earlier vanishes
// from the sidebar while still standing in the world. reconcilePlayers asks the
// game itself; this is the parsing of its answer.
func TestParsePlayerList(t *testing.T) {
	// The first three are copied byte for byte from the deployed fleet, ANSI
	// codes included. The first version of the parser accepted only the vanilla
	// sentence and matched none of them, so the reconcile shipped dead.
	const paperZero = "\x1b[33mThere are \x1b[31m0\x1b[33m out of maximum \x1b[31m20\x1b[33m players online.\n\x1b[0m"
	const essentialsNoOne = paperZero + "\x1b[0m\x1b[31mError:\x1b[31m There's no one online in this group!\n\x1b[0m"
	const velocity = "2026/08/15 02:31:09 Failed to connect to RCON serverrcon: authentication failed"

	cases := []struct {
		name string
		out  string
		want []string
		ok   bool
	}{
		{"paper, nobody on", paperZero, []string{}, true},
		{"paper via essentials", essentialsNoOne, []string{}, true},
		{"proxy has no rcon", velocity, nil, false},

		// EssentialsX answers the count on one line and the names on the next,
		// one line per permission group. Copied byte for byte from the deployed
		// Lobby with a player actually online - the case the first parser
		// refused, which is every case that mattered.
		{
			"paper via essentials, one on",
			"\x1b[33mThere are \x1b[31m1\x1b[33m out of maximum \x1b[31m20\x1b[33m players online.\n\x1b[0m\x1b[33mdefault\x1b[0m: Steve_Example\n\x1b[0m",
			[]string{"Steve_Example"}, true,
		},
		{
			"essentials, two groups",
			"There are 3 out of maximum 20 players online.\nadmins: Alice\ndefault: Bob, Steve_Example\n",
			[]string{"Alice", "Bob", "Steve_Example"}, true,
		},
		// The count is the check. A reply whose names do not add up to what the
		// server said is refused outright rather than half-believed.
		{"essentials, names disagree with count", "There are 4 out of maximum 20 players online.\ndefault: Alice\n", nil, false},

		{"vanilla, two on", "There are 2 of a max of 20 players online: Alice, Steve_Example\n", []string{"Alice", "Steve_Example"}, true},
		{"vanilla, one on", "There are 1 of a max of 200 players online: Steve_Example", []string{"Steve_Example"}, true},
		{"vanilla, nobody", "There are 0 of a max of 20 players online:\n", []string{}, true},

		// Plugins decorate names; take the name and drop the decoration.
		{"decorated", "There are 1 of a max of 20 players online: Steve_Example (afk)", []string{"Steve_Example"}, true},

		// A count with no names is not an answer to "who is online". Keeping
		// what the console said beats replacing it with a number.
		{"count without names", "There are 3 out of maximum 20 players online.", nil, false},

		{"not the sentence", "rcon-cli: executable file not found in $PATH", nil, false},
		{"empty", "", nil, false},
	}
	for _, c := range cases {
		got, ok := parsePlayerList(c.out)
		if ok != c.ok {
			t.Errorf("%s: ok=%v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}
