package core

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// newTestCommand builds a cobra command carrying every flag NewCrawler reads,
// with the same defaults as main.go.
func newTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "gospider-test"}
	flags := cmd.Flags()
	flags.Bool("quiet", true, "")
	flags.Bool("json", false, "")
	flags.Int("depth", 3, "")
	flags.Int("concurrent", 5, "")
	flags.Int("delay", 0, "")
	flags.Int("random-delay", 0, "")
	flags.Int("timeout", 10, "")
	flags.Bool("length", false, "")
	flags.Bool("raw", false, "")
	flags.Bool("subs", false, "")
	flags.String("proxy", "", "")
	flags.Bool("no-redirect", false, "")
	flags.String("burp", "", "")
	flags.String("cookie", "", "")
	flags.StringArray("header", nil, "")
	flags.String("user-agent", "web", "")
	flags.String("output", "", "")
	flags.String("filter-length", "", "")
	flags.String("blacklist", "", "")
	flags.String("whitelist", "", "")
	flags.String("whitelist-domain", "", "")
	return cmd
}

func mustParseSite(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u
}

// crawlSite runs one crawler to completion (main + JS collectors included).
func crawlSite(t *testing.T, rawSite string, budget *RequestBudget, linkfinder bool) *Crawler {
	t.Helper()
	cmd := newTestCommand(t)
	crawler := NewCrawler(mustParseSite(t, rawSite), cmd, budget)
	crawler.Start(linkfinder)
	crawler.C.Wait()
	crawler.LinkFinderCollector.Wait()
	return crawler
}

// siteServer serves a small inter-linked page plus a JS file and robots/sitemap.
func siteServer(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	count := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(hits, 1)
			next(w, r)
		}
	}
	mux.HandleFunc("/", count(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>
<a href="/page1">p1</a>
<a href="/page2">p2</a>
<a href="/page1">p1-dup</a>
<script src="/app.js"></script>
<img src="/logo.png">
</body></html>`)
	}))
	mux.HandleFunc("/page1", count(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><a href="/page3">p3</a></body></html>`)
	}))
	mux.HandleFunc("/page2", count(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><a href="/page3">p3</a></body></html>`)
	}))
	mux.HandleFunc("/page3", count(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>leaf</body></html>"))
	}))
	mux.HandleFunc("/app.js", count(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		// Disallowed static asset (.css) must never be requested.
		_, _ = w.Write([]byte(`var x = "/deep1"; var y = "/style.css";`))
	}))
	mux.HandleFunc("/deep1", count(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>deep</body></html>"))
	}))
	mux.HandleFunc("/robots.txt", count(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAllow: /from-robots\n"))
	}))
	mux.HandleFunc("/from-robots", count(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>robots</body></html>"))
	}))
	mux.HandleFunc("/sitemap.xml", count(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>FROM-SITEMAP-PLACEHOLDER/from-sitemap</loc></url>
</urlset>`))
	}))
	mux.HandleFunc("/from-sitemap", count(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>sitemap</body></html>"))
	}))
	return httptest.NewServer(mux)
}

// TestCrawlUnlimitedBaseline confirms the full crawl still works end to end
// when no budget is configured and every distinct entry is requested once.
func TestCrawlUnlimitedBaseline(t *testing.T) {
	var hits int64
	srv := siteServer(t, &hits)
	defer srv.Close()

	crawler := crawlSite(t, srv.URL+"/", NewRequestBudget(0), true)
	if crawler.Budget() == nil {
		t.Fatal("crawler should expose its budget")
	}
	// /, /page1, /page2, /page3, /app.js, /deep1 = 6 unique requests.
	want := int64(6)
	if got := atomic.LoadInt64(&hits); got != want {
		t.Fatalf("server hits = %d, want %d", got, want)
	}
	if crawler.Budget().WasRejected() {
		t.Fatal("unlimited crawl must never reject")
	}
}

// TestCrawlBudgetOne allows only the seed request: every further visit must be
// refused before reaching the network, and in-flight requests cannot exceed N.
func TestCrawlBudgetOne(t *testing.T) {
	var hits int64
	srv := siteServer(t, &hits)
	defer srv.Close()

	budget := NewRequestBudget(1)
	crawlSite(t, srv.URL+"/", budget, true)

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
	if budget.Used() != 1 {
		t.Fatalf("used = %d, want 1", budget.Used())
	}
	if !budget.WasRejected() {
		t.Fatal("expected budget rejections for the queued follow-up requests")
	}
}

// TestCrawlBudgetPartial caps the crawl halfway: exactly N requests are sent,
// rejections happen, and already collected results are retained.
func TestCrawlBudgetPartial(t *testing.T) {
	var hits int64
	srv := siteServer(t, &hits)
	defer srv.Close()

	const n int64 = 3
	budget := NewRequestBudget(n)
	crawlSite(t, srv.URL+"/", budget, false)

	if got := atomic.LoadInt64(&hits); got != n {
		t.Fatalf("server hits = %d, want %d", got, n)
	}
	if budget.Used() != n {
		t.Fatalf("used = %d, want %d", budget.Used(), n)
	}
	if budget.Rejected() == 0 {
		t.Fatal("partial crawl should record at least one refusal")
	}
}

// TestCrawlBudgetExactFit picks a limit large enough for the whole crawl: all
// requests complete, nothing is rejected and the run is considered complete.
func TestCrawlBudgetExactFit(t *testing.T) {
	var hits int64
	srv := siteServer(t, &hits)
	defer srv.Close()

	budget := NewRequestBudget(6)
	crawlSite(t, srv.URL+"/", budget, true)

	if got := atomic.LoadInt64(&hits); got != 6 {
		t.Fatalf("server hits = %d, want 6", got)
	}
	if budget.Used() != 6 {
		t.Fatalf("used = %d, want 6", budget.Used())
	}
	if budget.WasRejected() {
		t.Fatalf("exact-fit crawl must not reject, got %d refusals", budget.Rejected())
	}
}

// TestMultiSeedSharedBudget runs two crawlers (two "seeds") against one budget
// and proves the total request count across both never exceeds N.
func TestMultiSeedSharedBudget(t *testing.T) {
	var hitsA, hitsB int64
	srvA := siteServer(t, &hitsA)
	srvB := siteServer(t, &hitsB)
	defer srvA.Close()
	defer srvB.Close()

	const n int64 = 5
	budget := NewRequestBudget(n)
	c1 := NewCrawler(mustParseSite(t, srvA.URL+"/"), newTestCommand(t), budget)
	c2 := NewCrawler(mustParseSite(t, srvB.URL+"/"), newTestCommand(t), budget)

	c1.Start(true)
	c2.Start(true)
	c1.C.Wait()
	c2.C.Wait()
	c1.LinkFinderCollector.Wait()
	c2.LinkFinderCollector.Wait()

	total := atomic.LoadInt64(&hitsA) + atomic.LoadInt64(&hitsB)
	if total != n {
		t.Fatalf("combined hits = %d, want %d", total, n)
	}
	if budget.Used() != n {
		t.Fatalf("used = %d, want %d", budget.Used(), n)
	}
	if !budget.WasRejected() {
		t.Fatal("shared budget should have rejected at least one queued request")
	}
	// Each seed got at least its initial request.
	if atomic.LoadInt64(&hitsA) < 1 || atomic.LoadInt64(&hitsB) < 1 {
		t.Fatal("both seeds should have been able to make their seed request")
	}
}

// TestRobotsEntryCharged drives the robots.txt entry point through the budget.
func TestRobotsEntryCharged(t *testing.T) {
	var hits int64
	srv := siteServer(t, &hits)
	defer srv.Close()

	// N=1: robots.txt consumes the only slot, so its discovered URL is
	// refused without extra traffic. Start() is deliberately not called: the
	// robots entry point is the only contender for the budget here.
	budget := NewRequestBudget(1)
	cmd := newTestCommand(t)
	crawler := NewCrawler(mustParseSite(t, srv.URL), cmd, budget)

	runHelper(t, func(done func()) {
		ParseRobots(mustParseSite(t, srv.URL), crawler, crawler.C, helperWG(done))
	})
	crawler.C.Wait()

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("server hits = %d, want 1 (only robots.txt)", got)
	}
	if !budget.WasRejected() {
		t.Fatal("visit discovered from robots.txt should be rejected once the budget is full")
	}
}

// TestSitemapEntryCharged drives the sitemap entry point through the budget.
func TestSitemapEntryCharged(t *testing.T) {
	var hits int64
	srv := siteServer(t, &hits)
	defer srv.Close()

	// N=1: sitemap.xml itself consumes the slot; every discovered URL visit is
	// then refused at the transport layer.
	budget := NewRequestBudget(1)
	cmd := newTestCommand(t)
	crawler := NewCrawler(mustParseSite(t, srv.URL), cmd, budget)

	runHelper(t, func(done func()) {
		ParseSiteMap(mustParseSite(t, srv.URL), crawler, crawler.C, helperWG(done))
	})
	crawler.C.Wait()
	crawler.LinkFinderCollector.Wait()

	// Only the first sitemap candidate path (/sitemap.xml) is fetched; the
	// loop stops on the budget refusal before the remaining 11 candidates.
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("server hits = %d, want 1 (only /sitemap.xml)", got)
	}
	if !budget.WasRejected() {
		t.Fatal("sitemap-discovered visits should be rejected once budget is full")
	}
}

// TestSitemapFullFlow verifies that with enough budget the sitemap entry works
// and discovered URLs are actually visited.
func TestSitemapFullFlow(t *testing.T) {
	var hits int64
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		origin := "http://" + r.Host
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>%s/from-sitemap</loc></url>
</urlset>`, origin)
	})
	mux.HandleFunc("/from-sitemap", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = w.Write([]byte("<html>ok</html>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	budget := NewRequestBudget(50)
	cmd := newTestCommand(t)
	crawler := NewCrawler(mustParseSite(t, srv.URL+"/"), cmd, budget)
	runHelper(t, func(done func()) {
		ParseSiteMap(mustParseSite(t, srv.URL+"/"), crawler, crawler.C, helperWG(done))
	})
	crawler.C.Wait()

	// /sitemap.xml + /from-sitemap, and ParseSiteMap keeps probing the other
	// candidate paths (all 404) until the list ends; count only the two
	// meaningful ones.
	if got := atomic.LoadInt64(&hits); got < 2 {
		t.Fatalf("server hits = %d, want at least 2", got)
	}
	if budget.WasRejected() {
		t.Fatal("generous budget must not reject")
	}
}

// TestFilterAndDedupNotCounted proves URL-filter and de-duplication rejections
// never reach the transport and therefore consume no budget.
func TestFilterAndDedupNotCounted(t *testing.T) {
	var hits int64
	srv := siteServer(t, &hits)
	defer srv.Close()

	budget := NewRequestBudget(100)
	cmd := newTestCommand(t)
	crawler := NewCrawler(mustParseSite(t, srv.URL+"/"), cmd, budget)

	// Duplicate visit of the seed: colly marks the URL visited during the
	// first asynchronous scrape, and the second one is de-duplicated inside
	// requestCheck before any RoundTrip. Both calls may return nil because the
	// collector is asynchronous; the observable contract is zero traffic.
	_ = crawler.C.Visit(srv.URL + "/")
	_ = crawler.C.Visit(srv.URL + "/")
	// Filter rejection: .png is in the disallowed static-asset regex.
	err := crawler.C.Visit(srv.URL + "/logo.png")
	if err == nil {
		t.Fatal("disallowed URL should be rejected by URL filters")
	}
	crawler.C.Wait()

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("server hits = %d, want 1 (de-dup must suppress the repeat)", got)
	}
	if budget.Used() != 1 {
		t.Fatalf("filter/dedup rejections must not count: used = %d, want 1", budget.Used())
	}
	if budget.WasRejected() {
		t.Fatal("filter/dedup rejections must not flag budget refusal")
	}
}

// TestRedirectHopsCounted verifies redirect chains are charged hop by hop via
// the collector's transport and an exhausted budget truncates mid-chain.
func TestRedirectHopsCounted(t *testing.T) {
	var hits int64
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.Redirect(w, r, "/r1", http.StatusFound)
	})
	mux.HandleFunc("/r1", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.Redirect(w, r, "/r2", http.StatusFound)
	})
	mux.HandleFunc("/r2", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Only two slots: the chain reaches /r1 then the third hop is refused.
	budget := NewRequestBudget(2)
	cmd := newTestCommand(t)
	crawler := NewCrawler(mustParseSite(t, srv.URL+"/start"), cmd, budget)
	crawler.Start(false)
	crawler.C.Wait()

	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("server hits = %d, want 2 hops", got)
	}
	if budget.Used() != 2 {
		t.Fatalf("used = %d, want 2", budget.Used())
	}
	if !budget.WasRejected() {
		t.Fatal("third redirect hop should have been rejected")
	}
}

// TestFailedRequestCharged points a crawler at a dead server: the failed
// RoundTrip still consumes its slot, and the next attempt is budget-refused.
func TestFailedRequestCharged(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	budget := NewRequestBudget(1)
	cmd := newTestCommand(t)
	crawler := NewCrawler(mustParseSite(t, deadURL+"/"), cmd, budget)
	crawler.Start(false)
	crawler.C.Wait()

	if budget.Used() != 1 {
		t.Fatalf("failed seed request must be charged: used = %d, want 1", budget.Used())
	}

	// A second attempt must now be refused at the transport, synchronously in
	// this case because the collector can no longer schedule any network work.
	if err := crawler.C.Visit(deadURL + "/other"); err != nil {
		// Synchronous refusal (e.g. de-dup path) is fine as long as no slot is
		// used; the key assertions below cover the observable contract.
		t.Logf("second Visit returned: %v", err)
	}
	crawler.C.Wait()

	if budget.Used() != 1 {
		t.Fatalf("no further slot may be granted after exhaustion: used = %d, want 1", budget.Used())
	}
	if !budget.WasRejected() {
		t.Fatal("follow-up after a charged failure should be refused when budget is full")
	}
}
