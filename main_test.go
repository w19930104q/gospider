package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jaeles-project/gospider/core"
	"github.com/spf13/cobra"
)

// TestRunCLIIntegration drives the cobra-backed run() against an in-process
// gated HTTP server, proving the CLI entry point waits for the whole crawl
// (HTML -> JS -> HTML alternation, rings, robots, sitemap) and finalizes the
// output file before returning.
func TestRunCLIIntegration(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("/", page(`<html><body>
<a href="/a">a</a><a href="/b">b</a>
<script src="/app.js"></script></body></html>`))
	mux.HandleFunc("/a", page(`<a href="/b">b</a><a href="/c">c</a><a href="/a">ring</a>`))
	mux.HandleFunc("/b", page(`<a href="/a">ring</a><a href="/c">c</a>`))
	mux.HandleFunc("/c", page(`<a href="/a">ring</a>`))
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(`var x = "/d"; var y = "/ring.js";`))
	})
	mux.HandleFunc("/ring.js", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(`var back = "/a"; var again = "/app.js";`))
	})
	mux.HandleFunc("/d", page(`<a href="/c">c</a>`))
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("User-agent: *\nAllow: /\nAllow: /robpath\n"))
	})
	mux.HandleFunc("/robpath", page(`<a href="/a">a</a>`))
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/xml")
		body := `<?xml version="1.0"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>` + "http://" + r.Host + `/smpath</loc></url></urlset>`
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/smpath", page(`ok`))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()

	// Use a fresh command instance with the production flag set so the test is
	// isolated from the package-level command's arg/state.
	cmd := &cobra.Command{Use: core.CLIName, Run: run}
	registerFlags(cmd)
	args := []string{
		"--site", srv.URL + "/",
		"--output", dir,
		"--concurrent", "4",
		"--depth", "3",
		"--sitemap",
		"--robots",
		"--js",
		"--quiet",
	}
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Fatal("server recorded no requests")
	}

	// The output file must exist and contain the crawled URLs.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one output file, got %d: %v", len(entries), entries)
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	content := string(data)
	for _, want := range []string{"/a", "/b", "/c", "/app.js", "/ring.js", "/robpath", "/smpath"} {
		if !strings.Contains(content, want) {
			t.Errorf("output missing crawled path %q", want)
		}
	}
}
