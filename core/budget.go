package core

import (
	"errors"
	"net/http"
	"sync/atomic"
)

// ErrBudgetExhausted is returned by a BudgetTransport when the shared
// request budget has already been consumed and no further request is allowed.
var ErrBudgetExhausted = errors.New("request budget exhausted: max-requests limit reached")

// RequestBudget is a counter shared by every seed, collector and helper
// goroutine of a single gospider run. A limit of 0 (or any negative value
// treated as unlimited by the caller) disables limiting.
//
// Counting happens at the http.RoundTripper boundary so every actual network
// attempt is charged exactly once: failed attempts, upper-layer retries and
// every redirect hop each consume a slot, while requests rejected earlier by
// filters or de-duplication never reach the transport and are not counted.
type RequestBudget struct {
	limit    int64
	used     int64
	rejected int64
}

// NewRequestBudget creates a budget allowing at most limit RoundTrip calls.
// A limit <= 0 means unlimited.
func NewRequestBudget(limit int64) *RequestBudget {
	return &RequestBudget{limit: limit}
}

// Acquire reserves one request slot. It returns true when the slot is granted
// and false when the budget has been exhausted. Acquire is safe for concurrent
// use and never blocks, so callers keep their original concurrency instead of
// being serialised behind a global lock. Unlimited budgets still record usage
// so the total number of attempts remains observable.
func (b *RequestBudget) Acquire() bool {
	if b == nil {
		return true
	}
	if b.limit <= 0 {
		atomic.AddInt64(&b.used, 1)
		return true
	}
	for {
		current := atomic.LoadInt64(&b.used)
		if current >= b.limit {
			atomic.AddInt64(&b.rejected, 1)
			return false
		}
		if atomic.CompareAndSwapInt64(&b.used, current, current+1) {
			return true
		}
	}
}

// Used reports how many request slots have been granted so far.
func (b *RequestBudget) Used() int64 {
	if b == nil {
		return 0
	}
	return atomic.LoadInt64(&b.used)
}

// Rejected reports how many attempts were refused because the budget was full.
func (b *RequestBudget) Rejected() int64 {
	if b == nil {
		return 0
	}
	return atomic.LoadInt64(&b.rejected)
}

// WasRejected reports whether at least one attempt has been refused.
func (b *RequestBudget) WasRejected() bool {
	return b.Rejected() > 0
}

// Limit returns the configured limit (0 means unlimited).
func (b *RequestBudget) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit
}

// BudgetTransport wraps an http.RoundTripper and charges every RoundTrip call
// against the shared RequestBudget. When the budget is full it fails the
// request with ErrBudgetExhausted without touching the network.
//
// http.Client invokes RoundTrip once per redirect hop, so redirects are
// charged hop by hop. The Go transport's own transparent retries of
// idempotent requests happen inside the underlying RoundTripper and therefore
// are not counted again.
type BudgetTransport struct {
	Base   http.RoundTripper
	Budget *RequestBudget
}

// RoundTrip implements http.RoundTripper.
func (t *BudgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.Budget.Acquire() {
		return nil, ErrBudgetExhausted
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// crawlerTransport returns the transport to use for a collector. When the
// budget is unlimited the original transport is returned untouched, preserving
// the previous behaviour byte for byte.
func crawlerTransport(base http.RoundTripper, budget *RequestBudget) http.RoundTripper {
	if budget == nil || budget.Limit() <= 0 {
		return base
	}
	return &BudgetTransport{Base: base, Budget: budget}
}

// BudgetHTTPClient builds an *http.Client for helper goroutines (robots.txt and
// sitemap fetching) that shares the crawler's request budget. It wraps the
// given transport rather than touching http.DefaultTransport or
// http.DefaultClient. A nil budget yields a client with the original
// behaviour.
func (crawler *Crawler) BudgetHTTPClient() *http.Client {
	return &http.Client{Transport: crawlerTransport(DefaultHTTPTransport, crawler.budget)}
}

// Budget exposes the shared request budget of a crawler.
func (crawler *Crawler) Budget() *RequestBudget {
	return crawler.budget
}
