package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/astradns/astradns-agent/pkg/proxy"
)

func TestNewQueryLogger(t *testing.T) {
	logger := NewQueryLogger(LoggerConfig{Mode: LogModeOff})
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestRunFullModeLogsEveryEvent(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger := newQueryLogger(LoggerConfig{Mode: LogModeFull}, buffer)

	events := make(chan proxy.QueryEvent, 3)
	events <- sampleEvent("NOERROR")
	events <- sampleEvent("NOERROR")
	events <- sampleEvent("NOERROR")
	close(events)

	logger.Run(context.Background(), events)

	lines := nonEmptyLines(buffer.String())
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines, got %d", len(lines))
	}

	for _, line := range lines {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("expected valid JSON log line, got error: %v", err)
		}
	}
}

func TestRunSampledModeLogsHalfOfSuccessEvents(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger := newQueryLogger(LoggerConfig{Mode: LogModeSampled, SampleRate: 0.5}, buffer)

	events := make(chan proxy.QueryEvent, 10)
	for i := 0; i < 10; i++ {
		events <- sampleEvent("NOERROR")
	}
	close(events)

	logger.Run(context.Background(), events)

	lines := nonEmptyLines(buffer.String())
	if len(lines) != 5 {
		t.Fatalf("expected 5 log lines for 50%% sampling, got %d", len(lines))
	}
}

func TestRunErrorsOnlyModeLogsOnlyErrors(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger := newQueryLogger(LoggerConfig{Mode: LogModeErrorsOnly}, buffer)

	events := make(chan proxy.QueryEvent, 2)
	events <- sampleEvent("NOERROR")
	events <- sampleEvent("NXDOMAIN")
	close(events)

	logger.Run(context.Background(), events)

	lines := nonEmptyLines(buffer.String())
	if len(lines) != 1 {
		t.Fatalf("expected exactly one log line, got %d", len(lines))
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &payload); err != nil {
		t.Fatalf("expected valid JSON line, got error: %v", err)
	}
	if payload["rcode"] != "NXDOMAIN" {
		t.Fatalf("expected NXDOMAIN rcode, got %#v", payload["rcode"])
	}
}

func TestRunSampledModeAlwaysLogsErrors(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger := newQueryLogger(LoggerConfig{Mode: LogModeSampled, SampleRate: 0.01}, buffer)

	events := make(chan proxy.QueryEvent, 2)
	events <- sampleEvent("NOERROR")
	events <- sampleEvent("NXDOMAIN")
	close(events)

	logger.Run(context.Background(), events)

	lines := nonEmptyLines(buffer.String())
	if len(lines) != 1 {
		t.Fatalf("expected only error event to be logged, got %d lines", len(lines))
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &payload); err != nil {
		t.Fatalf("expected valid JSON line, got error: %v", err)
	}
	if payload["rcode"] != "NXDOMAIN" {
		t.Fatalf("expected NXDOMAIN rcode, got %#v", payload["rcode"])
	}
}

func TestRunOffModeLogsNothing(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger := newQueryLogger(LoggerConfig{Mode: LogModeOff}, buffer)

	events := make(chan proxy.QueryEvent, 2)
	events <- sampleEvent("NOERROR")
	events <- sampleEvent("NXDOMAIN")
	close(events)

	logger.Run(context.Background(), events)

	if output := strings.TrimSpace(buffer.String()); output != "" {
		t.Fatalf("expected no output in off mode, got %q", output)
	}
}

func sampleEvent(rcode string) proxy.QueryEvent {
	return proxy.QueryEvent{
		SourceIP:     "10.244.2.5",
		Domain:       "api.stripe.com",
		QueryType:    "A",
		ResponseCode: rcode,
		Upstream:     "1.1.1.1",
		LatencyMs:    12.3,
		CacheHit:     false,
	}
}

func nonEmptyLines(content string) []string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}
