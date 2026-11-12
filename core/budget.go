package core

import (
	"errors"
	"net/http"
	"sync"
)

// ErrRequestBudgetExhausted is returned by the budget transport when the
// per-site request budget has been spent. The request is never sent.
var ErrRequestBudgetExhausted = errors.New("request budget exhausted")

// RequestBudget is a per-input-site allowance of HTTP request attempts.
//
// Every HTTP request attempt made for one input site is charged against its
// own budget: HTML pages, Javascript files, robots.txt, sitemap files and
// every redirect hop all share the same counter. Failed attempts (DNS,
// connection, timeout, ...) are charged too, because the attempt happened.
// Requests that are dropped before being sent (deduplicated, filtered out by
// scope/extension rules, over depth, or aborted once the budget is spent) are
// never charged.
//
// A nil *RequestBudget means "unlimited" and all its methods are safe no-ops,
// so call sites do not need nil checks.
type RequestBudget struct {
	limit int64

	mu        sync.Mutex
	used      int64
	exhausted bool // set once at least one request had to be refused
}

// NewRequestBudget returns a budget of limit attempts, or nil (unlimited)
// when limit <= 0.
func NewRequestBudget(limit int) *RequestBudget {
	if limit <= 0 {
		return nil
	}
	return &RequestBudget{limit: int64(limit)}
}

// TryAcquire reserves one HTTP request attempt. It returns false when the
// budget is already spent; every refusal marks the budget exhausted so the
// crawl is reported as truncated. It never blocks, so concurrent crawlers
// can never spend more than limit attempts in total.
func (b *RequestBudget) TryAcquire() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.limit {
		b.exhausted = true
		return false
	}
	b.used++
	return true
}

// Depleted reports whether no attempts remain.
func (b *RequestBudget) Depleted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used >= b.limit
}

// MarkExhausted records that a request was dropped because of the budget,
// without charging an attempt (the request was never sent).
func (b *RequestBudget) MarkExhausted() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.exhausted = true
	b.mu.Unlock()
}

// Exhausted reports whether the budget ever refused a request, i.e. whether
// the crawl was truncated because of the budget.
func (b *RequestBudget) Exhausted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exhausted
}

// Used returns the number of attempts charged so far.
func (b *RequestBudget) Used() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Limit returns the configured limit (0 for a nil, unlimited budget).
func (b *RequestBudget) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit
}

// BudgetSummary is the per-site closing report emitted when
// --request-budget is in effect.
type BudgetSummary struct {
	Input     string `json:"input"`
	Type      string `json:"type"`
	Budget    int64  `json:"budget"`
	Used      int64  `json:"used"`
	Truncated bool   `json:"truncated"`
}

// Summary builds the closing report for one input site: how many attempts
// were charged and whether the crawl was cut short by the budget.
func (b *RequestBudget) Summary(input string) BudgetSummary {
	return BudgetSummary{
		Input:     input,
		Type:      "request-budget",
		Budget:    b.Limit(),
		Used:      b.Used(),
		Truncated: b.Exhausted(),
	}
}

// budgetTransport charges every HTTP attempt against a RequestBudget before
// it hits the wire. Because it sits at the RoundTripper level, it also
// covers redirect hops followed by the http.Client and attempts that fail.
type budgetTransport struct {
	base   http.RoundTripper
	budget *RequestBudget
}

func (t *budgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.budget.TryAcquire() {
		return nil, ErrRequestBudgetExhausted
	}
	return t.base.RoundTrip(req)
}

// WrapBudgetTransport wraps base so every attempt is charged to budget.
// A nil budget returns base unchanged (original behavior).
func WrapBudgetTransport(base http.RoundTripper, budget *RequestBudget) http.RoundTripper {
	if budget == nil {
		return base
	}
	return &budgetTransport{base: base, budget: budget}
}
