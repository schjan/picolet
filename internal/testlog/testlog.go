// Package testlog records slog output in tests, so a test can hand a component
// its own logger and assert the records it produced without touching the
// process-wide slog default.
package testlog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// Record is one captured log line: level, message and its attributes rendered
// as key=value.
type Record struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

// Recorder is a slog.Handler that keeps every record at any level.
// It is safe for concurrent use; loggers derived via With/WithGroup share it.
type Recorder struct {
	mu      *sync.Mutex
	records *[]Record
	attrs   []slog.Attr
	group   string
}

// New returns a Recorder and a logger writing to it.
func New() (*slog.Logger, *Recorder) {
	r := &Recorder{mu: &sync.Mutex{}, records: &[]Record{}}
	return slog.New(r), r
}

// Enabled implements slog.Handler; every level is recorded.
func (r *Recorder) Enabled(context.Context, slog.Level) bool { return true }

// Handle implements slog.Handler.
func (r *Recorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := make(map[string]string, rec.NumAttrs()+len(r.attrs))
	for _, a := range r.attrs {
		attrs[r.key(a.Key)] = a.Value.String()
	}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[r.key(a.Key)] = a.Value.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.records = append(*r.records, Record{Level: rec.Level, Message: rec.Message, Attrs: attrs})
	return nil
}

func (r *Recorder) key(k string) string {
	if r.group == "" {
		return k
	}
	return r.group + "." + k
}

// WithAttrs implements slog.Handler.
func (r *Recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *r
	c.attrs = append(append([]slog.Attr(nil), r.attrs...), attrs...)
	return &c
}

// WithGroup implements slog.Handler.
func (r *Recorder) WithGroup(name string) slog.Handler {
	c := *r
	c.group = c.key(name)
	return &c
}

// Records returns a copy of every record captured so far.
func (r *Recorder) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), *r.records...)
}

// Has reports whether a record at level carries message.
func (r *Recorder) Has(level slog.Level, message string) bool {
	for _, rec := range r.Records() {
		if rec.Level == level && rec.Message == message {
			return true
		}
	}
	return false
}

// Count returns the number of records at level.
func (r *Recorder) Count(level slog.Level) int {
	n := 0
	for _, rec := range r.Records() {
		if rec.Level == level {
			n++
		}
	}
	return n
}

// String renders the captured records one per line, for failure messages.
func (r *Recorder) String() string {
	var b strings.Builder
	for _, rec := range r.Records() {
		fmt.Fprintf(&b, "%s %q %v\n", rec.Level, rec.Message, rec.Attrs)
	}
	return b.String()
}
