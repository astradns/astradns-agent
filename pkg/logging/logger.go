package logging

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync/atomic"

	"github.com/astradns/astradns-agent/pkg/proxy"
)

// LogMode determines the query logging strategy.
type LogMode string

const (
	// LogModeFull logs every DNS query event.
	LogModeFull LogMode = "full"
	// LogModeSampled logs sampled DNS query events.
	LogModeSampled LogMode = "sampled"
	// LogModeErrorsOnly logs only error responses.
	LogModeErrorsOnly LogMode = "errors-only"
	// LogModeOff disables DNS query logging.
	LogModeOff LogMode = "off"
)

// LoggerConfig configures query logging behavior.
type LoggerConfig struct {
	Mode       LogMode
	SampleRate float64
}

// QueryLogger logs DNS query events as structured JSON.
type QueryLogger struct {
	config        LoggerConfig
	logger        *slog.Logger
	sampleCounter atomic.Uint64
	sampleEvery   uint64
}

// NewQueryLogger creates a QueryLogger that writes JSON logs to stdout.
func NewQueryLogger(config LoggerConfig) *QueryLogger {
	return newQueryLogger(config, os.Stdout)
}

// Run consumes events from the channel and logs them.
func (l *QueryLogger) Run(ctx context.Context, events <-chan proxy.QueryEvent) {
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			l.logEvent(event)
		case <-ctx.Done():
			for event := range events {
				l.logEvent(event)
			}
			return
		}
	}
}

func (l *QueryLogger) logEvent(event proxy.QueryEvent) {
	if !l.shouldLog(event) {
		return
	}

	l.logger.Info(
		"dns_query",
		slog.String("source_ip", event.SourceIP),
		slog.String("namespace", "unknown"),
		slog.String("domain", event.Domain),
		slog.String("qtype", event.QueryType),
		slog.String("rcode", event.ResponseCode),
		slog.String("upstream", event.Upstream),
		slog.Float64("latency_ms", event.LatencyMs),
		slog.Bool("cache_hit_known", event.CacheHitKnown),
		slog.Bool("cache_hit", event.CacheHit),
	)
}

func newQueryLogger(config LoggerConfig, writer io.Writer) *QueryLogger {
	if config.Mode == "" {
		config.Mode = LogModeSampled
	}

	if config.SampleRate < 0 {
		config.SampleRate = 0
	}
	if config.SampleRate > 1 {
		config.SampleRate = 1
	}

	logger := slog.New(slog.NewJSONHandler(writer, nil))
	l := &QueryLogger{config: config, logger: logger}

	if config.Mode == LogModeSampled {
		if config.SampleRate <= 0 {
			l.sampleEvery = 0
		} else {
			l.sampleEvery = uint64(math.Round(1 / config.SampleRate))
			if l.sampleEvery == 0 {
				l.sampleEvery = 1
			}
		}
	}

	return l
}

func (l *QueryLogger) shouldLog(event proxy.QueryEvent) bool {
	if l.config.Mode == LogModeOff {
		return false
	}

	if isErrorCode(event.ResponseCode) {
		return true
	}

	switch l.config.Mode {
	case LogModeFull:
		return true
	case LogModeErrorsOnly:
		return false
	case LogModeSampled:
		if l.sampleEvery == 0 {
			return false
		}
		if l.sampleEvery == 1 {
			return true
		}
		counter := l.sampleCounter.Add(1)
		return counter%l.sampleEvery == 0
	default:
		return false
	}
}

func isErrorCode(responseCode string) bool {
	code := strings.ToUpper(responseCode)
	return code == "NXDOMAIN" || code == "SERVFAIL"
}
