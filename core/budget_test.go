package core

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRequestBudgetUnlimited(t *testing.T) {
	b := NewRequestBudget(0)
	for i := 0; i < 1000; i++ {
		if !b.Acquire() {
			t.Fatalf("unlimited budget rejected request %d", i)
		}
	}
	if b.WasRejected() {
		t.Fatal("unlimited budget recorded a rejection")
	}
	if b.Used() != 1000 {
		t.Fatalf("used = %d, want 1000", b.Used())
	}

	// nil budget must behave as unlimited as well.
	var nilBudget *RequestBudget
	if !nilBudget.Acquire() {
		t.Fatal("nil budget should always grant")
	}
}

func TestRequestBudgetExactLimit(t *testing.T) {
	const limit int64 = 5
	b := NewRequestBudget(limit)
	for i := int64(0); i < limit; i++ {
		if !b.Acquire() {
			t.Fatalf("request %d should have been granted", i)
		}
	}
	// The budget was consumed exactly: every further attempt is refused.
	for i := 0; i < 3; i++ {
		if b.Acquire() {
			t.Fatal("request past the limit should have been rejected")
		}
	}
	if b.Used() != limit {
		t.Fatalf("used = %d, want %d", b.Used(), limit)
	}
	if b.Rejected() != 3 {
		t.Fatalf("rejected = %d, want 3", b.Rejected())
	}
	if !b.WasRejected() {
		t.Fatal("WasRejected must be true after a refusal")
	}
}

// TestRequestBudgetConcurrentSharesOneCounter hammers the budget from many
// goroutines: exactly N acquisitions must succeed and concurrency must never
// exceed N, without any global serialisation of the underlying requests.
func TestRequestBudgetConcurrentSharesOneCounter(t *testing.T) {
	const (
		limit   int64 = 20
		workers       = 50
		perG          = 10
	)
	b := NewRequestBudget(limit)
	var granted int64
	var inFlight, maxInFlight int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if !b.Acquire() {
					continue
				}
				cur := atomic.AddInt64(&inFlight, 1)
				for {
					m := atomic.LoadInt64(&maxInFlight)
					if cur <= m || atomic.CompareAndSwapInt64(&maxInFlight, m, cur) {
						break
					}
				}
				atomic.AddInt64(&granted, 1)
				atomic.AddInt64(&inFlight, -1)
			}
		}()
	}
	wg.Wait()

	if granted != limit {
		t.Fatalf("granted = %d, want %d", granted, limit)
	}
	if b.Used() != limit {
		t.Fatalf("used = %d, want %d", b.Used(), limit)
	}
	if b.Rejected() != int64(workers*perG)-limit {
		t.Fatalf("rejected = %d, want %d", b.Rejected(), int64(workers*perG)-limit)
	}
	if maxInFlight > limit {
		t.Fatalf("max in-flight = %d exceeds limit %d", maxInFlight, limit)
	}
}

// TestBudgetTransportCountsEveryHop uses a redirect chain to prove that each
// RoundTrip hop is charged, including the final one, and that the failure of
// the last hop still consumed its slot.
func TestBudgetTransportCountsEveryHop(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/r1", http.StatusFound)
		case "/r1":
			http.Redirect(w, r, "/r2", http.StatusFound)
		case "/r2":
			http.Redirect(w, r, "/final", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("done"))
		}
	}))
	defer srv.Close()

	// Four hops (/start, /r1, /r2, /final) need four slots.
	b := NewRequestBudget(4)
	client := &http.Client{Transport: &BudgetTransport{Base: http.DefaultTransport, Budget: b}}
	resp, err := client.Get(srv.URL + "/start")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if b.Used() != 4 {
		t.Fatalf("used = %d, want 4 (one per redirect hop)", b.Used())
	}
	if atomic.LoadInt64(&hits) != 4 {
		t.Fatalf("server saw %d hits, want 4", hits)
	}
	if b.WasRejected() {
		t.Fatal("exact-limit run must not record a rejection")
	}

	// One slot left: the second request is refused before touching the network.
	resp2, err := client.Get(srv.URL + "/start")
	if err == nil {
		_ = resp2.Body.Close()
		t.Fatal("expected ErrBudgetExhausted")
	}
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("want ErrBudgetExhausted, got %v", err)
	}
	if atomic.LoadInt64(&hits) != 4 {
		t.Fatalf("refused request reached the server: hits = %d", hits)
	}
	if !b.WasRejected() {
		t.Fatal("refusal must be recorded")
	}
}

// TestBudgetTransportChargesFailures proves a failed RoundTrip (connection
// refused) still consumes a slot, and filter/dedup style rejections that never
// reach the transport do not.
func TestBudgetTransportChargesFailures(t *testing.T) {
	// Close the listener immediately: every dial fails.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	b := NewRequestBudget(2)
	client := &http.Client{Transport: &BudgetTransport{Base: http.DefaultTransport, Budget: b}}
	for i := 0; i < 2; i++ {
		if _, err := client.Get(deadURL); err == nil {
			t.Fatal("expected connection failure")
		}
	}
	if b.Used() != 2 {
		t.Fatalf("failed attempts must be charged: used = %d, want 2", b.Used())
	}
	if _, err := client.Get(deadURL); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("want ErrBudgetExhausted, got %v", err)
	}
	if b.Rejected() != 1 {
		t.Fatalf("rejected = %d, want 1", b.Rejected())
	}
}

// TestCrawlerTransportUnlimitedUnwrapped ensures no behaviour change when the
// budget is disabled: the original transport is returned as-is and the global
// default transport is never replaced.
func TestCrawlerTransportUnlimitedUnwrapped(t *testing.T) {
	base := http.DefaultTransport
	if got := crawlerTransport(base, NewRequestBudget(0)); got != http.RoundTripper(base) {
		t.Fatal("unlimited budget must keep the original transport unwrapped")
	}
	if got := crawlerTransport(base, nil); got != http.RoundTripper(base) {
		t.Fatal("nil budget must keep the original transport unwrapped")
	}
	got := crawlerTransport(base, NewRequestBudget(3))
	bt, ok := got.(*BudgetTransport)
	if !ok || bt.Base != base {
		t.Fatal("limited budget must wrap, not replace, the original transport")
	}
}
