package logring

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestKeeps(t *testing.T) {
	l := Logger(slog.New(slog.NewTextHandler(io.Discard, nil))).With("job", "j1")
	if Logger(l).Handler() != l.Handler() {
		t.Fatal("wrapped twice")
	}
	l.Debug("not kept")
	l.Info("backup started", "files", 3)
	for i := 0; i < Lines*2; i++ {
		l.Warn("filler")
	}
	l.Error("the last line")
	text := Text()
	if strings.Contains(text, "not kept") || strings.Contains(text, "backup started") {
		t.Fatal("kept a debug line, or more lines than it should")
	}
	if !strings.HasSuffix(text, "msg=\"the last line\" job=j1\n") || strings.Count(text, "\n") > Lines+Lines/4 {
		t.Fatalf("wrong end or size: %d lines, ends %q", strings.Count(text, "\n"), text[len(text)-80:])
	}
}
