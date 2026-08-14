package arcade

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// overflowingPort is 2^64 + 25600. The hand-rolled atoi computed n*10+digit with
// no bound, so this 20-digit string wrapped to exactly 25600 - a legal, ordinary
// Minecraft port, and deliberately one no seeded server holds, so the assertions
// below turn on the overflow rather than on a port conflict.
const overflowingPort = "18446744073709577216"

// L9. itoa took the sign off with i = -i before walking the digits, which is a
// no-op at math.MinInt: the loop condition i > 0 was false on the first pass, no
// digits were emitted, and the caller got a bare "-". defaultProps and
// Manager.Create both run every port and max-players value through it on the way
// into server.properties, so the corrupt value would have been written to disk.
func TestItoaFormatsTheMostNegativeInt(t *testing.T) {
	for _, i := range []int{math.MinInt, math.MinInt + 1, math.MaxInt, -1, 0, 1, -25565} {
		want := fmt.Sprintf("%d", i)
		if got := itoa(i); got != want {
			t.Errorf("itoa(%d) = %q, want %q", i, got, want)
		}
	}
}

// L8. atoi returns 0 for anything it cannot use, and every caller guards on that
// - `if p > 0`, or a range check. Overflow was the one input that broke the
// contract: the value wrapped instead, so a number far too large for an int came
// back as a small in-range one and passed every guard.
func TestAtoiRefusesNumbersTooLargeForAnInt(t *testing.T) {
	// Each of these wrapped to a different nonzero value, so none of the
	// assertions can pass by accident against the old implementation.
	cases := []string{
		overflowingPort,        // wrapped to 25600
		"99999999999999999999", // wrapped to 7766279631452241919
		"-99999999999999999999",
		"9223372036854775808", // MaxInt64 + 1, wrapped to MinInt64
	}
	for _, in := range cases {
		if got := atoi(in); got != 0 {
			t.Errorf("atoi(%q) = %d, want 0 - the value wrapped into the usable range", in, got)
		}
	}

	// The inputs that already worked must keep working; callers depend on the
	// 0-for-garbage answer as much as on the numeric one.
	for in, want := range map[string]int{
		"25565": 25565, " 20 ": 20, "-5": -5, "0": 0, "": 0, "abc": 0, "3.5": 0, "12a": 0,
	} {
		if got := atoi(in); got != want {
			t.Errorf("atoi(%q) = %d, want %d", in, got, want)
		}
	}
}

// L8, at the caller the overflow actually reaches. ApplySettings range-checks
// the port it gets back from atoi, so the wraparound walked straight through the
// check: the panel accepted a 20-digit port, wrote it verbatim into
// server.properties, and recorded the server as listening on 25600 - a port the
// container is never told to bind, and one the conflict check now believes is
// taken.
func TestApplySettingsRejectsAnOverflowingPort(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0] // nothing is started, so these fields have no other writer
	before := s.Port

	if _, err := mgr.ApplySettings(s, map[string]string{"server-port": overflowingPort}); err == nil {
		t.Fatalf("server-port %q was accepted", overflowingPort)
	}
	if s.Port != before {
		t.Errorf("port = %d, want %d - a rejected value must not half-apply", s.Port, before)
	}
	if got := s.Props["server-port"]; got == overflowingPort {
		t.Error("the rejected port was written into server.properties anyway")
	}
}

// L8, the other live caller. reloadProps reads a hand-edited server.properties
// back into the panel's model with no range check at all, trusting atoi to
// report an unusable value as 0. Before the fix an oversized server-port silently
// became the server's port, and the panel and the file agreed on a number the
// operator never typed.
func TestReloadPropsIgnoresAnOverflowingPort(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	port, max := s.Port, s.MaxPlayers

	mgr.reloadProps(s, "server-port="+overflowingPort+"\nmax-players="+overflowingPort+"\n")

	if s.Port != port {
		t.Errorf("port = %d, want %d - an unparseable port must leave the model alone", s.Port, port)
	}
	if s.MaxPlayers != max {
		t.Errorf("max_players = %d, want %d", s.MaxPlayers, max)
	}
}

// L10. The template directory was created 0o755 while the files the panel cares
// about are 0o600, so on a shared host any other account could list and read the
// set of images the panel launches game servers from.
func TestTemplatesDirIsNotWorldReadable(t *testing.T) {
	// LoadTemplates publishes into a package-level set that the rest of the
	// suite reads through allTemplates().
	tplMu.RLock()
	saved := tplLoaded
	tplMu.RUnlock()
	t.Cleanup(func() {
		tplMu.Lock()
		tplLoaded = saved
		tplMu.Unlock()
	})

	dir := t.TempDir()
	if err := LoadTemplates(dir); err != nil {
		t.Fatalf("load templates: %v", err)
	}

	st, err := os.Stat(filepath.Join(dir, "templates"))
	if err != nil {
		t.Fatalf("stat templates dir: %v", err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("templates dir mode = %v, want no group or world bits", perm)
	}
}
