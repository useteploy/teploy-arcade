package arcade

import (
	"os"
	"strings"
	"testing"
	"time"
)

// H11. Lifecycle read the resulting status back with b.m.Get(id).State() and no
// nil check, after the start/stop/restart had already been applied. A server
// deleted in that window makes Get return nil and the deref panic: the agent is
// handed a broken response for an action that did take effect, so a retrying
// agent issues a duplicate start or stop.
func TestMCPLifecycleSurvivesAConcurrentDelete(t *testing.T) {
	_, mgr := newTestAgent(t)
	b := mcpBackend{m: mgr}
	id := mgr.List()[0].ID

	if err := mgr.Start(id); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, 12*time.Second, func() bool { return mgr.Get(id).State() == StatusRunning })

	type outcome struct {
		msg     string
		err     error
		crashed any
	}
	res := make(chan outcome, 1)

	// Audit.Append holds this lock across its whole marshal-and-write, so
	// taking it first pins Lifecycle between the action and the status
	// read-back. That makes the delete land inside the window on every run
	// instead of once in a thousand.
	mgr.auth.mu.Lock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				res <- outcome{crashed: r}
			}
		}()
		msg, err := b.Lifecycle(id, "stop")
		res <- outcome{msg: msg, err: err}
	}()

	// The status leaves Running inside Stop, which is after Lifecycle's own
	// lookup already succeeded - so from here on this is the concurrent delete
	// the finding describes, not merely an unknown id.
	waitFor(t, 5*time.Second, func() bool {
		s := mgr.Get(id)
		return s != nil && s.State() != StatusRunning
	})
	// Delete refuses to touch a server that is still stopping, so drop it from
	// the map directly: the one effect that matters here is Get returning nil.
	mgr.mu.Lock()
	delete(mgr.servers, id)
	mgr.mu.Unlock()
	mgr.auth.mu.Unlock()

	select {
	case got := <-res:
		if got.crashed != nil {
			t.Fatalf("Lifecycle panicked on a server deleted mid-call: %v", got.crashed)
		}
		if got.err != nil {
			t.Fatalf("Lifecycle: %v", got.err)
		}
		if !strings.Contains(got.msg, "stop requested") {
			t.Errorf("Lifecycle reported %q; the stop did happen and has to be reported as such", got.msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Lifecycle never returned")
	}
}

// M15. The bearer-token hash was compared with ==, which returns at the first
// differing byte and leaks how much of a guessed token was right; the password
// path next door already uses a constant-time compare. Timing itself cannot be
// asserted reliably in a unit test, so this pins the comparison and the
// behaviour it must not break.
func TestMCPTokenCompareIsConstantTime(t *testing.T) {
	_, mgr := newTestAgent(t)
	tok, err := mgr.mcp.Issue("agent")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !mgr.mcp.Check(tok) {
		t.Fatal("the issued token was rejected")
	}
	near := tok[:len(tok)-1] + "0"
	if strings.HasSuffix(tok, "0") {
		near = tok[:len(tok)-1] + "1"
	}
	for _, bad := range []string{"", "tpa_", near, tok + "a"} {
		if mgr.mcp.Check(bad) {
			t.Errorf("token %q was accepted", bad)
		}
	}

	src, err := os.ReadFile("mcp.go")
	if err != nil {
		t.Fatalf("read mcp.go: %v", err)
	}
	body := funcSourceAfter(string(src), "func (t *mcpTokens) Check(")
	if body == "" {
		t.Fatal("could not find mcpTokens.Check in mcp.go")
	}
	if !strings.Contains(body, "subtle.ConstantTimeCompare") {
		t.Error("Check does not compare the stored hash in constant time; == returns early and leaks the matching prefix")
	}
}

// funcSourceAfter returns the source of the function whose signature starts
// with sig, up to its closing brace.
func funcSourceAfter(src, sig string) string {
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	body := src[i:]
	if j := strings.Index(body, "\n}\n"); j > 0 {
		body = body[:j]
	}
	return body
}

// M12. arcade_send_command forwarded any text straight to the game console. The
// agent is told to read its results back from that same console, and console
// output is written by players, plugins and the MOTD - so an injected line could
// talk the agent into op-ing an attacker or stopping the server. The verbs that
// hand out or remove access are refused now, and a line break can no longer
// smuggle a second command past that check.
func TestMCPRefusesAccessChangingConsoleCommands(t *testing.T) {
	_, mgr := newTestAgent(t)
	b := mcpBackend{m: mgr}
	id := mgr.List()[0].ID

	if err := mgr.Start(id); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, 12*time.Second, func() bool { return mgr.Get(id).State() == StatusRunning })

	for _, cmd := range []string{
		"op attacker", "/OP attacker", " deop admin", "whitelist off",
		"ban someone", "stop", "say hello\nop attacker",
	} {
		if out, err := b.SendCommand(id, cmd); err == nil {
			t.Errorf("SendCommand(%q) was accepted: %s", cmd, out)
		}
	}

	// Refused has to mean not delivered, not just not reported: the console
	// echo is what an operator would see happen.
	for _, l := range mgr.hub.Tail(id, 500) {
		if strings.Contains(strings.ToLower(l.Text), "op attacker") {
			t.Fatalf("a refused command still reached the console: %q", l.Text)
		}
	}
	for _, e := range mgr.auth.Audit(200) {
		if e.Action == "console.command" && strings.Contains(strings.ToLower(e.Detail), "op attacker") {
			t.Fatalf("a refused command was audited as sent: %q", e.Detail)
		}
	}

	// Ordinary commands still go through; this is a refusal list, not a lockout.
	if _, err := b.SendCommand(id, "say hello"); err != nil {
		t.Fatalf("an ordinary command was refused: %v", err)
	}
}
