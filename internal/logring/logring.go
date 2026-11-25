// Package logring keeps the most recent log lines of this process in
// memory, so that the web UI can show them without anyone having to
// find a log file on the machine.
package logring

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
)

// Lines is how many lines are kept.
const Lines = 3000

var (
	mu    sync.Mutex
	lines []string
)

type handler struct {
	slog.Handler
	attrs []slog.Attr
}

// Wrap returns a handler that passes records on to h and also keeps
// them. Wrapping a wrapped handler changes nothing.
func Wrap(h slog.Handler) slog.Handler {
	if _, ok := h.(*handler); ok {
		return h
	}
	return &handler{Handler: h}
}

// Logger wraps a logger's handler; nil means the default logger.
func Logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	return slog.New(Wrap(l.Handler()))
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || h.Handler.Enabled(ctx, l)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo {
		var b bytes.Buffer
		th := slog.NewTextHandler(&b, nil).WithAttrs(h.attrs)
		if th.Handle(ctx, r) == nil {
			mu.Lock()
			lines = append(lines, strings.TrimRight(b.String(), "\n"))
			if len(lines) > Lines+Lines/4 {
				lines = append([]string(nil), lines[len(lines)-Lines:]...)
			}
			mu.Unlock()
		}
	}
	if h.Handler.Enabled(ctx, r.Level) {
		return h.Handler.Handle(ctx, r)
	}
	return nil
}

func (h *handler) WithAttrs(a []slog.Attr) slog.Handler {
	return &handler{Handler: h.Handler.WithAttrs(a), attrs: append(append([]slog.Attr(nil), h.attrs...), a...)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{Handler: h.Handler.WithGroup(name), attrs: h.attrs}
}

// Text returns the kept lines, oldest first.
func Text() string {
	mu.Lock()
	defer mu.Unlock()
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
