package core

import (
	"sync"
	"testing"
)

// helperWG turns a completion callback into the *sync.WaitGroup expected by
// ParseRobots/ParseSiteMap.
func helperWG(done func()) *sync.WaitGroup {
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		wg.Wait()
		done()
	}()
	return wg
}

// runHelper executes a robots/sitemap helper goroutine and waits for it.
func runHelper(t *testing.T, fn func(done func())) {
	t.Helper()
	finished := make(chan struct{})
	fn(func() { close(finished) })
	<-finished
}
