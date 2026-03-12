package processlog

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLineWriterLogsProcessLines(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	writer := newLineWriter("unbound", "stderr", logger, 10, time.Minute, func() time.Time {
		return time.Unix(0, 0)
	})

	_, _ = writer.Write([]byte("first line\nsecond line\n"))

	got := output.String()
	if !strings.Contains(got, "first line") {
		t.Fatalf("expected first line in logs, got: %s", got)
	}
	if !strings.Contains(got, "second line") {
		t.Fatalf("expected second line in logs, got: %s", got)
	}
}

func TestLineWriterSuppressesWhenRateLimitExceeded(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	writer := newLineWriter("coredns", "stdout", logger, 2, time.Minute, func() time.Time {
		return time.Unix(0, 0)
	})

	_, _ = writer.Write([]byte("line 1\nline 2\nline 3\nline 4\n"))

	got := output.String()
	if !strings.Contains(got, "line 1") || !strings.Contains(got, "line 2") {
		t.Fatalf("expected first two lines to be logged, got: %s", got)
	}
	if strings.Contains(got, "line 3") || strings.Contains(got, "line 4") {
		t.Fatalf("expected lines beyond limit to be suppressed, got: %s", got)
	}
	if !strings.Contains(got, "engine process output suppressed") {
		t.Fatalf("expected suppression warning, got: %s", got)
	}
}
