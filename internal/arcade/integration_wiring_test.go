package arcade

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Integration-phase regressions: two features were built in parallel against a
// shared mux and a shared capability list, and one scan field was returned
// inverted. All three only fail against the assembled panel, which is the one
// configuration no feature author had.

// The feature agents each registered their routes on a fresh mux inside their
// own tests, so both suites passed while the shipping mux had neither. A route
// nothing registers is a 404 at runtime and a green test suite - the failure
// this whole phase exists to catch.
func TestPluginsAndImportAreOnTheShippingMux(t *testing.T) {
	hub := NewHub()
	mgr := NewManager(t.TempDir(), hub)
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]

	// Routes(), not RoutesPlugins/RoutesImport: the point is that the entry
	// point the binary calls reaches them.
	mux := http.NewServeMux()
	(&API{mgr: mgr, hub: hub}).Routes(mux)
	srv := httptest.NewServer(mgr.auth.attach(mux))
	defer srv.Close()

	for _, tc := range []struct {
		method, path string
		body         string
	}{
		{"GET", "/api/servers/" + s.ID + "/plugins", ""},
		{"POST", "/api/servers/" + s.ID + "/plugins/toggle", `{"file":"x.jar","enable":false}`},
		{"DELETE", "/api/servers/" + s.ID + "/plugins?file=x.jar", ""},
		{"POST", "/api/servers/" + s.ID + "/plugins/install", `{"url":"http://example.invalid/x.jar"}`},
		{"POST", "/api/import/scan", `{"path":"/nope"}`},
		{"POST", "/api/import", `{"path":"/nope"}`},
		{"GET", "/api/import/nosuchjob", ""},
	} {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if code, body, missing := readRoute(resp); missing {
			t.Errorf("%s %s: %d %q - not registered by Routes()",
				tc.method, tc.path, code, body)
		}
	}
}

// readRoute answers whether the mux, rather than a handler, produced this
// response.
//
// An unregistered pattern and a handler that answers "no such import job" are
// both 404, so the status alone cannot tell them apart. Only the mux writes
// these plain-text bodies; every handler here writes JSON.
//
// 405 counts as missing too, and is the likelier shape: with RoutesPlugins
// gone, "POST /api/servers/{id}/{action}" still matches the *path* of GET and
// DELETE /api/servers/{id}/plugins, so the mux answers Method Not Allowed
// instead of 404 - and a 404-only check waves the missing route straight
// through.
func readRoute(resp *http.Response) (code int, body string, missing bool) {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body = strings.TrimSpace(string(b))
	code = resp.StatusCode
	missing = (code == 404 && strings.Contains(body, "404 page not found")) ||
		(code == 405 && strings.Contains(body, "Method Not Allowed"))
	return code, body, missing
}

// capabilities is how the UI and any MCP consumer decide whether to offer a
// feature. Shipping the plugin routes while still answering "plugins": false
// hides a working feature from every client that asks first.
func TestCapabilitiesAgreeWithTheRegisteredRoutes(t *testing.T) {
	hub := NewHub()
	mgr := NewManager(t.TempDir(), hub)
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := mgr.List()[0]

	mux := http.NewServeMux()
	(&API{mgr: mgr, hub: hub}).Routes(mux)
	srv := httptest.NewServer(mgr.auth.attach(mux))
	defer srv.Close()

	var caps struct {
		Features map[string]bool `json:"features"`
	}
	cr, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer cr.Body.Close()
	if err := json.NewDecoder(cr.Body).Decode(&caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}

	// Agreement alone is a tautology: delete the routes AND flip the flag to
	// false and a served==advertised check still passes, reporting green on a
	// feature that no longer exists. So each shipped feature is asserted to be
	// advertised true AND actually served - agreement then falls out of both.
	shipped := []struct {
		feature string
		probe   string
	}{
		{"plugins", "/api/servers/" + s.ID + "/plugins"},
		{"files", "/api/servers/" + s.ID + "/files"},
		{"backups", "/api/servers/" + s.ID + "/backups"},
		{"metrics", "/api/servers/" + s.ID + "/metrics"},
		{"audit", "/api/audit"},
		{"import", "/api/import/no-such-job"},
		// The route half of scheduled backups. The behavioural half - that
		// `!backup` in a task actually produces an archive - is asserted in
		// TestScheduledBackupsCapabilityMatchesTheScheduler, because a task
		// route proves a scheduler exists, not that the action works.
		{"scheduled_backups", "/api/servers/" + s.ID + "/tasks"},
	}

	for _, c := range shipped {
		if !caps.Features[c.feature] {
			t.Errorf("capabilities says %q is off, but it ships", c.feature)
		}
		resp, err := http.Get(srv.URL + c.probe)
		if err != nil {
			t.Fatalf("%s: %v", c.probe, err)
		}
		code, body, missing := readRoute(resp)
		if missing {
			t.Errorf("%q ships but %s is not registered (%d %q)",
				c.feature, c.probe, code, body)
		}
	}

	// And a feature that is honestly off stays off, so the map is not just
	// every key set to true. disk_quota is the honest one now: the panel warns
	// and refuses a create the disk cannot hold, but nothing enforces a
	// per-server allowance at the filesystem layer, and on ext4 inside an LXC
	// nothing can.
	for _, off := range []string{"disk_quota"} {
		if caps.Features[off] {
			t.Errorf("capabilities advertises %q, which is not implemented", off)
		}
	}
}

// measureTree reports whether it finished; SizePartial is the opposite. The two
// were wired straight across, so a scan that measured every byte still reported
// itself as a floor - a four-file directory came back as "at least 117 B", and
// the disk-space check was reasoning about a number the UI had just disowned.
func TestACompleteScanIsNotReportedAsPartial(t *testing.T) {
	_, mgr := newImportAgent(t)
	dir := mkTree(t, "small", map[string]string{
		"paper-1.20.4.jar":  "PK\x03\x04 jar",
		"server.properties": "server-port=25599\nmotd=Small\n",
		"world/level.dat":   "level",
	})

	sc, err := mgr.ScanImport(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sc.SizePartial {
		t.Errorf("a 3-file directory reported size_partial=true; the walk finished")
	}
	if strings.HasPrefix(sc.SizeHuman, "at least") {
		t.Errorf("size_human = %q; a complete measurement is not a floor", sc.SizeHuman)
	}
	if sc.SizeBytes == 0 || sc.Files != 3 {
		t.Errorf("measured %d bytes over %d files; want 3 files and a nonzero size",
			sc.SizeBytes, sc.Files)
	}
}
