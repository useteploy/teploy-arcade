package arcade

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The panel sets a WriteTimeout so a stalled client cannot hold a connection
// open forever. That deadline is absolute, not idle-based, so it also applies
// to responses that are legitimately long: a world download over a slow link, a
// backup archive, an SSE feed that stays open for hours. Those handlers lift it
// explicitly.
//
// This drives the real download route through a real http.Server with a short
// WriteTimeout and a client that reads slowly - the situation an operator on a
// slow link is actually in. A unit test on clearStreamDeadlines by itself would
// pass with every call site deleted.
//
// The payload has to be larger than the socket buffers, or the server's io.Copy
// returns immediately, the client reads from its own buffer and no deadline is
// ever exercised. The control case at the bottom fails the test if that
// happens, rather than letting it pass for the wrong reason.
const (
	streamTestBytes  = 8 << 20 // past any plausible socket buffer
	streamTestWrite  = 300 * time.Millisecond
	streamTestChunks = 12
	streamTestPause  = 80 * time.Millisecond // ~1s total, well past the timeout
)

// slowRead pulls a body in chunks with a pause between them, so the response is
// still in flight long after the write deadline would have fired.
func slowRead(url string) (int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	buf := make([]byte, streamTestBytes/streamTestChunks)
	total := 0
	for {
		n, err := resp.Body.Read(buf)
		total += n
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		time.Sleep(streamTestPause)
	}
}

func TestSlowDownloadsOutliveTheWriteTimeout(t *testing.T) {
	_, mgr := newTestAgent(t)
	s := mgr.List()[0]
	if err := mgr.WriteFile(s, "world/region.mca", strings.Repeat("x", streamTestBytes)); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	(&API{mgr: mgr, hub: mgr.hub}).Routes(mux)
	srv := httptest.NewUnstartedServer(mgr.auth.attach(mux))
	srv.Config.WriteTimeout = streamTestWrite
	srv.Start()
	defer srv.Close()

	got, err := slowRead(srv.URL + "/api/servers/" + s.ID + "/download?path=world/region.mca")
	if err != nil {
		t.Fatalf("a slow download was cut off: %v (%d of %d bytes)", err, got, streamTestBytes)
	}
	if got != streamTestBytes {
		t.Fatalf("slow download truncated: %d of %d bytes", got, streamTestBytes)
	}
}

// The control. Without the lift, the same slow read must NOT survive - that is
// what gives the test above its meaning. If this ever passes, the payload has
// grown small relative to the socket buffers, or WriteTimeout stopped applying,
// and the test above has gone vacuous.
func TestTheWriteTimeoutReallyBitesWithoutTheLift(t *testing.T) {
	body := strings.Repeat("x", streamTestBytes)
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately no clearStreamDeadlines.
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, strings.NewReader(body))
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.WriteTimeout = streamTestWrite
	srv.Start()
	defer srv.Close()

	got, err := slowRead(srv.URL + "/stream")
	if err == nil && got == streamTestBytes {
		t.Errorf("a %d-byte slow read completed under a %v WriteTimeout; "+
			"TestSlowDownloadsOutliveTheWriteTimeout is no longer proving anything",
			streamTestBytes, streamTestWrite)
	}
}
