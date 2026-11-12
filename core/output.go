package core

import (
	"os"
	"path/filepath"
	"sync"
)

type Output struct {
	mu sync.Mutex
	f  *os.File
}

func NewOutput(folder, filename string) *Output {
	outFile := filepath.Join(folder, filename)
	f, err := os.OpenFile(outFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err != nil {
		Logger.Errorf("Failed to open file to write Output: %s", err)
		os.Exit(1)
	}
	return &Output{
		f: f,
	}
}

func (o *Output) WriteToFile(msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, _ = o.f.WriteString(msg + "\n")
}

// WriteToFileQuiet behaves like WriteToFile but surfaces the write error. It
// exists mainly so tests can prove the file handle is closed after the crawl
// finishes.
func (o *Output) WriteToFileQuiet(msg string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := o.f.WriteString(msg + "\n")
	return err
}

// Close closes the underlying file. Crawler.Wait invokes it exactly once.
func (o *Output) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.f.Close()
}
