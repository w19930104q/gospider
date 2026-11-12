package core

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// Gated HTTP infrastructure
// ---------------------------------------------------------------------------

// gateTransport wraps the default transport so every request performed by the
// colly Collectors is recorded. It is the test-side proof that nothing talks to
// the network after Crawler.Wait has returned.
type gateTransport struct {
	base     http.RoundTripper
	mu       sync.Mutex
	activity []string
	// inFlight counts requests currently inside RoundTrip. A clean shutdown
	// must always drain this back to zero.
	inFlight int32
}

func (g *gateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&g.inFlight, 1)
	defer atomic.AddInt32(&g.inFlight, -1)
	g.mu.Lock()
	g.activity = append(g.activity, req.Method+" "+req.URL.String())
	g.mu.Unlock()
	return g.base.RoundTrip(req)
}

func (g *gateTransport) Snapshot() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.activity))
	copy(out, g.activity)
	return out
}

func (g *gateTransport) Inflight() int32 { return atomic.LoadInt32(&g.inFlight) }

// gatedServer records every request it receives and can deliberately delay a
// path ("slow script"). All responses are produced in-process, so the tests
// are fully offline and deterministic.
type gatedServer struct {
	srv       *httptest.Server
	mu        sync.Mutex
	requested []string
	slow      map[string]chan struct{}
}

func newGatedServer(handler http.HandlerFunc) *gatedServer {
	g := &gatedServer{slow: map[string]chan struct{}{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// gospider builds robots/sitemap URLs via site.String()+"/robots.txt",
		// which yields a double slash when the site ends with "/". Normalize so
		// both forms hit the same fixture entry.
		p := strings.Replace(r.URL.Path, "//", "/", 1)
		r.URL.Path = p
		g.mu.Lock()
		g.requested = append(g.requested, p)
		gate := g.slow[p]
		g.mu.Unlock()
		if gate != nil {
			// Block until the test releases the slow resource.
			<-gate
		}
		handler(w, r)
	}))
	return g
}

func (g *gatedServer) close() { g.srv.Close() }

func (g *gatedServer) setSlow(path string) chan struct{} {
	ch := make(chan struct{})
	g.mu.Lock()
	g.slow[path] = ch
	g.mu.Unlock()
	return ch
}

func (g *gatedServer) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.requested))
	copy(out, g.requested)
	return out
}

// ---------------------------------------------------------------------------
// Test fixture: a site with alternating HTML/JS derivation, rings, failures,
// duplicates and external redirects to filtered URLs.
// ---------------------------------------------------------------------------

// fixtureHandler serves the graph used by most tests:
//
//	/            -> /a, /b, /slow.js, /missing, external link (filtered)
//	/a           -> /b, /c            (HTML derives HTML)
//	/b           -> /a (ring), /app.js
//	/c           -> /a (ring), /b (duplicate)
//	/app.js      -> '/d?from=appjs', '/ring.js'        (JS derives HTML + JS)
//	/ring.js     -> '/a', '/app.js'                    (JS ring back into HTML)
//	/robots.txt  -> /robpath
//	/sitemap.xml -> /smpath
//	/slow.js     -> held back by a gate until the test releases it
//	/missing     -> 500
func fixtureHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch r.URL.Path {
	case "/":
		_, _ = w.Write([]byte(`<html><body>
<a href="/a">a</a>
<a href="/b">b</a>
<a href="/missing">missing</a>
<a href="http://example.com/offsite">offsite-filtered</a>
<script src="/slow.js"></script>
</body></html>`))
	case "/a":
		_, _ = w.Write([]byte(`<a href="/b">b</a><a href="/c">c</a><a href="/a">self-ring</a>`))
	case "/b":
		_, _ = w.Write([]byte(`<a href="/a">ring</a><script src="/app.js"></script>`))
	case "/c":
		_, _ = w.Write([]byte(`<a href="/a">ring</a><a href="/b">dup</a>`))
	case "/robpath":
		_, _ = w.Write([]byte(`<a href="/fromrobots">r</a>`))
	case "/fromrobots":
		_, _ = w.Write([]byte(`<a href="/a">a</a>`))
	case "/smpath":
		_, _ = w.Write([]byte(`<a href="/fromsitemap">s</a>`))
	case "/fromsitemap":
		_, _ = w.Write([]byte(`<a href="/c">c</a>`))
	case "/other1", "/other2":
		_, _ = w.Write([]byte(`<a href="/otherdeep">d</a>`))
	case "/otherdeep":
		_, _ = w.Write([]byte(`ok`))
	case "/app.js":
		w.Header().Set("Content-Type", "application/javascript")
		// linkfinder regex needs quoted, dotted/extension-like tokens
		_, _ = w.Write([]byte(`var x = "/d?from=appjs"; var y = '/ring.js';`))
	case "/ring.js":
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(`var back = "/a"; var again = '/app.js';`))
	case "/slow.js":
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(`var late = '/latepage';`))
	case "/latepage":
		_, _ = w.Write([]byte(`late`))
	case "/d":
		_, _ = w.Write([]byte(`<a href="/c">c</a>`))
	case "/robots.txt":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nDisallow: /missing\nAllow: /robpath\n"))
	case "/sitemap.xml":
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>` + baseOf(r) + `/smpath</loc></url>
</urlset>`))
	case "/missing":
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	default:
		// Everything else (including the many probed sitemap locations) is 404.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}
}

func baseOf(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// newTestCmd builds a cobra command carrying every flag NewCrawler reads.
func newTestCmd(quiet bool) *cobra.Command {
	cmd := &cobra.Command{Use: "gospider-test"}
	flags := []struct {
		name string
		def  interface{}
	}{
		{"quiet", quiet}, {"json", false}, {"depth", 3}, {"concurrent", 4},
		{"delay", 0}, {"random-delay", 0}, {"length", false}, {"raw", false},
		{"subs", false}, {"proxy", ""}, {"timeout", 5}, {"no-redirect", false},
		{"burp", ""}, {"cookie", ""}, {"header", []string{}},
		{"user-agent", "test-agent"}, {"output", ""}, {"filter-length", ""},
		{"blacklist", ""}, {"whitelist", ""}, {"whitelist-domain", ""},
	}
	for _, f := range flags {
		switch d := f.def.(type) {
		case int:
			cmd.Flags().Int(f.name, d, "")
		case bool:
			cmd.Flags().Bool(f.name, d, "")
		case string:
			cmd.Flags().String(f.name, d, "")
		case []string:
			cmd.Flags().StringArray(f.name, d, "")
		}
	}
	return cmd
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u
}

// newTestCrawler wires a Crawler against the gated server with the recording
// transport installed on both Collectors (Clone shares the HTTP backend).
func newTestCrawler(t *testing.T, g *gatedServer, outputFolder string) *Crawler {
	t.Helper()
	return newTestCrawlerOpts(t, g, outputFolder, true)
}

func newTestCrawlerOpts(t *testing.T, g *gatedServer, outputFolder string, quiet bool) *Crawler {
	t.Helper()
	cmd := newTestCmd(quiet)
	if outputFolder != "" {
		if err := cmd.Flags().Set("output", outputFolder); err != nil {
			t.Fatalf("set output flag: %v", err)
		}
	}
	cr := NewCrawler(mustParse(t, g.srv.URL+"/"), cmd)
	rt := &gateTransport{base: http.DefaultTransport}
	cr.C.WithTransport(rt)
	cr.activityTransport = rt
	return cr
}

// assertQuiet proves no further request is made after completion. It never
// decides completion itself - Run has already returned; this only verifies the
// post-condition (no fixed-wait / polling used for synchronization).
func assertQuiet(t *testing.T, rt *gateTransport) {
	t.Helper()
	if got := rt.Inflight(); got != 0 {
		t.Fatalf("transport still has %d in-flight requests after Wait", got)
	}
	before := len(rt.Snapshot())
	time.Sleep(150 * time.Millisecond)
	if after := len(rt.Snapshot()); after != before {
		t.Fatalf("network activity after completion: %d -> %d requests", before, after)
	}
	if got := rt.Inflight(); got != 0 {
		t.Fatalf("in-flight count non-zero after settle: %d", got)
	}
}

func pathSet(paths []string) map[string]bool {
	m := make(map[string]bool, len(paths))
	for _, p := range paths {
		m[p] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRunFullGraph covers the headline scenario: robots, sitemap, the main
// crawl and a stubbed other-source all contribute pages, HTML and JS derive
// each other alternately, a slow JS script is held back, rings terminate, a
// failing URL and duplicate URLs settle, and the full expected URL set is hit.
func TestRunFullGraph(t *testing.T) {
	g := newGatedServer(fixtureHandler)
	defer g.close()
	slowGate := g.setSlow("/slow.js")

	dir := t.TempDir()
	// Non-quiet so labeled output ([url], [sitemap], [robots] ...) is written
	// and can be asserted; quiet mode intentionally strips the labels.
	cr := newTestCrawlerOpts(t, g, dir, false)

	// Source double: third-party provider replaced offline, including one
	// duplicated URL and pages that themselves derive more work.
	cr.otherSourceFn = func(domain string, includeSubs bool) []string {
		return []string{g.srv.URL + "/other1", g.srv.URL + "/other2", g.srv.URL + "/other1"}
	}

	done := make(chan struct{})
	go func() {
		cr.Run(true, true, true, true, false, true)
		close(done)
	}()

	// Give the rest of the graph a head start while slow.js stays blocked:
	// proves an early-finishing source must not lose the pages it enqueued and
	// that completion does not depend on fixed sleeps.
	select {
	case <-done:
		t.Fatal("Run returned while slow script gate is still closed")
	case <-time.After(300 * time.Millisecond):
	}
	close(slowGate)

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not finish after slow gate released")
	}

	// ---- Assert complete URL set (order independent) -----------------------
	gotSet := pathSet(g.requests())
	wantPaths := []string{
		"/", "/a", "/b", "/c", "/d", "/missing",
		"/app.js", "/ring.js", "/slow.js", "/latepage",
		"/robots.txt", "/robpath", "/fromrobots",
		"/sitemap.xml", "/smpath", "/fromsitemap",
		"/other1", "/other2", "/otherdeep",
	}
	for _, p := range wantPaths {
		if !gotSet[p] {
			t.Errorf("expected path %q was never requested; got=%v", p, sortedKeys(gotSet))
		}
	}
	for _, p := range g.requests() {
		if strings.Contains(p, "example.com") {
			t.Errorf("filtered off-site URL was requested: %s", p)
		}
	}

	// ---- Output file exists, contains results and is closed exactly once ---
	outFile := filepath.Join(dir, strings.ReplaceAll(mustParse(t, g.srv.URL).Hostname(), ".", "_"))
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("output file missing after completion: %v", err)
	}
	content := string(data)
	for _, token := range []string{"[url]", "[sitemap]", "[robots]", "[other-sources]", "[linkfinder]"} {
		if !strings.Contains(content, token) {
			t.Errorf("output file missing token %q", token)
		}
	}
	// Writing after close must surface an error (closed exactly once at end).
	if err := cr.Output.WriteToFileQuiet("post-close"); err == nil {
		t.Error("output file was not closed after Wait")
	}

	assertQuiet(t, cr.activityTransport.(*gateTransport))
}

// TestEarlySourceNoLoss enqueues derived URLs before the main crawl has
// produced anything, then the source returns immediately. Its pages must still
// all be processed.
func TestEarlySourceNoLoss(t *testing.T) {
	g := newGatedServer(fixtureHandler)
	defer g.close()
	cr := newTestCrawler(t, g, "")
	cr.otherSourceFn = func(string, bool) []string {
		// Returns instantly with a page that derives further pages.
		return []string{g.srv.URL + "/other1"}
	}
	cr.Run(false, false, false, true, false, true)

	gotSet := pathSet(g.requests())
	for _, p := range []string{"/other1", "/otherdeep"} {
		if !gotSet[p] {
			t.Errorf("early source lost page %q; got=%v", p, sortedKeys(gotSet))
		}
	}
	assertQuiet(t, cr.activityTransport.(*gateTransport))
}

// TestFailuresAndRings points a source at a closed port (connection error) and
// crawls the cyclic HTML/JS graph. Failures must be accounted for (Wait must
// return) and rings must terminate.
func TestFailuresAndRings(t *testing.T) {
	g := newGatedServer(fixtureHandler)
	defer g.close()
	// A second, immediately-closed server gives a reliable connection error.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cr := newTestCrawler(t, g, t.TempDir())
	cr.otherSourceFn = func(string, bool) []string {
		return []string{deadURL + "/unreachable", g.srv.URL + "/a"}
	}
	finished := make(chan struct{})
	go func() {
		cr.Run(true, true, true, true, false, false)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("Wait never returned despite failing source URL and cyclic graph")
	}

	gotSet := pathSet(g.requests())
	for _, p := range []string{"/", "/a", "/b", "/c", "/app.js", "/ring.js"} {
		if !gotSet[p] {
			t.Errorf("ring graph missing %q; got=%v", p, sortedKeys(gotSet))
		}
	}
	counts := map[string]int{}
	for _, p := range g.requests() {
		counts[p]++
	}
	// colly's default storage dedups revisit; the ring must not explode.
	if counts["/a"] > 2 {
		t.Errorf("ring not bounded: /a requested %d times", counts["/a"])
	}
	assertQuiet(t, cr.activityTransport.(*gateTransport))
}

// TestParallelEntries runs several independent Crawlers concurrently to prove
// each entry settles on its own and that all output files are finalized.
func TestParallelEntries(t *testing.T) {
	const n = 3
	var wg sync.WaitGroup
	var failMu sync.Mutex
	var failures []string
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g := newGatedServer(fixtureHandler)
			defer g.close()
			dir, err := os.MkdirTemp("", "gospider-par-*")
			if err != nil {
				failMu.Lock()
				failures = append(failures, err.Error())
				failMu.Unlock()
				return
			}
			cr := newTestCrawler(t, g, dir)
			cr.otherSourceFn = func(string, bool) []string { return nil }
			cr.Run(true, true, true, true, false, false)
			rt := cr.activityTransport.(*gateTransport)
			if rt.Inflight() != 0 {
				failMu.Lock()
				failures = append(failures, "requests still in flight after Wait")
				failMu.Unlock()
				return
			}
			outFile := filepath.Join(dir, strings.ReplaceAll(mustParse(t, g.srv.URL).Hostname(), ".", "_"))
			if _, err := os.Stat(outFile); err != nil {
				failMu.Lock()
				failures = append(failures, "output file not finalized: "+err.Error())
				failMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failures) != 0 {
		t.Fatalf("parallel entries failed: %v", failures)
	}
}

// TestStartStandalone confirms the original Start entry still works and its
// enqueued work is tracked when wrapped via GoSource + Wait.
func TestStartStandalone(t *testing.T) {
	g := newGatedServer(fixtureHandler)
	defer g.close()
	cr := newTestCrawler(t, g, "")
	cr.GoSource(func() { cr.Start(true) })
	cr.Wait()

	gotSet := pathSet(g.requests())
	for _, p := range []string{"/app.js", "/ring.js", "/a"} {
		if !gotSet[p] {
			t.Errorf("standalone Start missed derived work %q; got=%v", p, sortedKeys(gotSet))
		}
	}
	assertQuiet(t, cr.activityTransport.(*gateTransport))
}
