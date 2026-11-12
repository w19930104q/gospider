package core

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCounterBalanceOnRejectsAndErrors proves the WaitGroup bookkeeping stays
// balanced for every terminal state colly can produce:
//   - duplicate URLs (colly rejects, released in visitCollector),
//   - filtered URLs (rejected up-front),
//   - connection errors (OnError releases),
//   - HTTP 500 (OnError releases).
//
// A leak would make Wait hang (test timeout) and an extra Done would panic.
func TestCounterBalanceOnRejectsAndErrors(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<a href="/x">x</a>`))
	}))
	defer ok.Close()

	// Reliable connection error: a server that is closed before requests.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	// Error status server.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer failSrv.Close()

	g := newGatedServer(fixtureHandler)
	defer g.close()
	cr := newTestCrawler(t, g, "")

	// 1) Duplicates: same accepted URL many times - all but the first rejected.
	for i := 0; i < 20; i++ {
		_ = cr.Visit(ok.URL + "/dup")
	}
	// 2) Filtered: off-scope URL rejected by URLFilters synchronously.
	for i := 0; i < 10; i++ {
		_ = cr.Visit("http://offscope.example.invalid/filtered")
	}
	// 3) Connection errors: OnError path on main and link-finder collectors.
	for i := 0; i < 5; i++ {
		_ = cr.Visit(deadURL + "/down")
		_ = cr.VisitLinkFinder(deadURL + "/down-js")
	}
	// 4) HTTP 500: response received but colly routes >=203 to OnError.
	for i := 0; i < 5; i++ {
		_ = cr.Visit(failSrv.URL + "/err")
	}

	finished := make(chan struct{})
	go func() {
		cr.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("Wait hung: counter not balanced (leak) across rejects/errors")
	}
	if got := cr.activityTransport.(*gateTransport).Inflight(); got != 0 {
		t.Fatalf("in-flight=%d after Wait", got)
	}
}

// TestRunIdempotent ensures calling Run twice cannot re-run or double-close.
func TestRunIdempotent(t *testing.T) {
	g := newGatedServer(fixtureHandler)
	defer g.close()
	cr := newTestCrawler(t, g, t.TempDir())
	cr.otherSourceFn = func(string, bool) []string { return nil }

	cr.Run(false, false, false, false, false, false)
	// Second call must return promptly without enqueuing anything new.
	before := len(g.requests())
	cr.Run(true, true, true, true, true, true)
	if after := len(g.requests()); after != before {
		t.Fatalf("second Run enqueued new requests: %d -> %d", before, after)
	}
	// File remains closed (no panic on repeated Wait close path).
	if err := cr.Output.WriteToFileQuiet("x"); err == nil {
		t.Fatal("output file should stay closed after the first Run")
	}
}
