package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestValidateOptions(t *testing.T) {
	cases := []struct {
		name        string
		maxRequests int64
		otherSource bool
		wantErr     bool
	}{
		{"unlimited", 0, false, false},
		{"positive budget", 10, false, false},
		{"negative invalid", -1, false, true},
		{"large negative invalid", -100, false, true},
		{"other source alone allowed", 0, true, false},
		{"budget plus other source rejected", 1, true, true},
		{"zero budget plus other source allowed", 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOptions(tc.maxRequests, tc.otherSource)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestCLIExitCodes drives the compiled binary against a local server and
// asserts the documented exit codes and stderr reporting.
func TestCLIExitCodes(t *testing.T) {
	if os.Getenv("GOSPIDER_HELPER_BIN") == "" {
		// Compile once outside the per-case subprocesses.
		dir := t.TempDir()
		binPath := filepath.Join(dir, "gospider")
		build := exec.Command("go", "build", "-o", binPath, ".")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build helper binary: %v\n%s", err, out)
		}
		t.Setenv("GOSPIDER_HELPER_BIN", binPath)
	}
	binPath := os.Getenv("GOSPIDER_HELPER_BIN")

	t.Run("negative rejected before network", func(t *testing.T) {
		cmd := exec.Command(binPath, "-s", "http://127.0.0.1:1/", "--max-requests", "-1", "-q")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			t.Fatal("negative --max-requests must exit non-zero")
		}
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("want exit code 1, got %v", err)
		}
		if !strings.Contains(stderr.String(), "max-requests") {
			t.Fatalf("stderr should explain the flag error, got: %s", stderr.String())
		}
	})

	t.Run("other-source conflict rejected before network", func(t *testing.T) {
		cmd := exec.Command(binPath, "-s", "http://127.0.0.1:1/", "--max-requests", "5", "--other-source", "-q")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			t.Fatalf("want exit code 1, got %v", err)
		}
		if !strings.Contains(stderr.String(), "other-source") {
			t.Fatalf("stderr should mention --other-source, got: %s", stderr.String())
		}
	})
}

// crawlServer hosts a root page linking to several unique paths.
func crawlServer(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>
<a href="/a">a</a><a href="/b">b</a><a href="/c">c</a>
<a href="/d">d</a><a href="/e">e</a>
</body></html>`)
	})
	for _, p := range []string{"/a", "/b", "/c", "/d", "/e"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(hits, 1)
			_, _ = w.Write([]byte("<html>leaf</html>"))
		})
	}
	return httptest.NewServer(mux)
}

// TestCLIBudgetExitCodes verifies the full binary: truncation exits 2 with a
// clear stderr message and partial results kept; an exact-fit run exits 0 with
// no truncation message; unlimited runs still exit 0.
func TestCLIBudgetExitCodes(t *testing.T) {
	if os.Getenv("GOSPIDER_HELPER_BIN") == "" {
		dir := t.TempDir()
		binPath := filepath.Join(dir, "gospider")
		build := exec.Command("go", "build", "-o", binPath, ".")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build helper binary: %v\n%s", err, out)
		}
		t.Setenv("GOSPIDER_HELPER_BIN", binPath)
	}
	binPath := os.Getenv("GOSPIDER_HELPER_BIN")

	var hits int64
	srv := crawlServer(t, &hits)
	defer srv.Close()

	t.Run("truncated exits 2 and keeps results", func(t *testing.T) {
		atomic.StoreInt64(&hits, 0)
		outDir := t.TempDir()
		cmd := exec.Command(binPath, "-s", srv.URL+"/", "--max-requests", "2",
			"-d", "2", "--js=false", "--robots=false", "-o", outDir)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, runErr := cmd.Output()

		if runErr == nil {
			t.Fatal("truncated run must exit non-zero")
		}
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 2 {
			t.Fatalf("want exit code 2, got %v", runErr)
		}
		if !strings.Contains(stderr.String(), "Truncated") {
			t.Fatalf("stderr must report truncation, got: %s", stderr.String())
		}
		if got := atomic.LoadInt64(&hits); got != 2 {
			t.Fatalf("server hits = %d, want exactly 2", got)
		}
		// Results gathered before exhaustion must be retained on stdout.
		if len(stdout) == 0 {
			t.Fatal("partial crawl results should still be printed")
		}
		// And persisted to the output folder with no leftover temp files.
		entries, err := os.ReadDir(outDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("want exactly one output file, got %d", len(entries))
		}
		data, err := os.ReadFile(filepath.Join(outDir, entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			t.Fatal("output file should contain the partial results")
		}
	})

	t.Run("exact fit completes with exit 0", func(t *testing.T) {
		atomic.StoreInt64(&hits, 0)
		// Six unique pages exist (/, /a.. /e); give exactly six slots.
		cmd := exec.Command(binPath, "-s", srv.URL+"/", "--max-requests", "6",
			"-d", "2", "--js=false", "--robots=false", "-q")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("exact-fit run should exit 0, got %v", err)
		}
		if strings.Contains(stderr.String(), "Truncated") {
			t.Fatalf("exact-fit run must not report truncation: %s", stderr.String())
		}
		if got := atomic.LoadInt64(&hits); got != 6 {
			t.Fatalf("server hits = %d, want 6", got)
		}
	})

	t.Run("unlimited completes with exit 0", func(t *testing.T) {
		atomic.StoreInt64(&hits, 0)
		cmd := exec.Command(binPath, "-s", srv.URL+"/",
			"-d", "2", "--js=false", "--robots=false", "-q")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("unlimited run should exit 0, got %v", err)
		}
		if strings.Contains(stderr.String(), "Truncated") {
			t.Fatal("unlimited run must never report truncation")
		}
		if got := atomic.LoadInt64(&hits); got != 6 {
			t.Fatalf("server hits = %d, want 6", got)
		}
	})

	t.Run("multiple seeds share one budget", func(t *testing.T) {
		var hitsA, hitsB int64
		srvA := crawlServer(t, &hitsA)
		srvB := crawlServer(t, &hitsB)
		defer srvA.Close()
		defer srvB.Close()

		// Two seeds, three slots total: aggregate traffic must be exactly 3
		// even though both seeds start crawling at once.
		sitesFile := filepath.Join(t.TempDir(), "sites.txt")
		if err := os.WriteFile(sitesFile,
			[]byte(srvA.URL+"/\n"+srvB.URL+"/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binPath, "-S", sitesFile,
			"--max-requests", "3", "-t", "2", "-d", "2",
			"--js=false", "--robots=false", "-q")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 2 {
			t.Fatalf("want exit code 2, got %v", err)
		}
		total := atomic.LoadInt64(&hitsA) + atomic.LoadInt64(&hitsB)
		if total != 3 {
			t.Fatalf("combined hits across seeds = %d, want 3", total)
		}
		if atomic.LoadInt64(&hitsA) < 1 || atomic.LoadInt64(&hitsB) < 1 {
			t.Fatal("both seeds must have made at least their seed request")
		}
		if !strings.Contains(stderr.String(), "Truncated") {
			t.Fatal("stderr must report truncation")
		}
	})
}
