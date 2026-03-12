package processlog

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultMaxLinesPerWindow = 200
	defaultWindowDuration    = time.Minute
	maxBufferedLineBytes     = 8 << 10
)

// NewLineWriter creates a rate-limited line writer for engine process output.
func NewLineWriter(engineName, stream string) io.Writer {
	return newLineWriter(
		engineName,
		stream,
		slog.Default(),
		defaultMaxLinesPerWindow,
		defaultWindowDuration,
		time.Now,
	)
}

type lineWriter struct {
	engineName string
	stream     string
	logger     *slog.Logger

	maxLinesPerWindow int
	windowDuration    time.Duration
	now               func() time.Time

	mu              sync.Mutex
	buffer          []byte
	windowStart     time.Time
	linesEmitted    int
	linesSuppressed int
}

func newLineWriter(
	engineName,
	stream string,
	logger *slog.Logger,
	maxLinesPerWindow int,
	windowDuration time.Duration,
	now func() time.Time,
) *lineWriter {
	if logger == nil {
		logger = slog.Default()
	}
	if maxLinesPerWindow <= 0 {
		maxLinesPerWindow = defaultMaxLinesPerWindow
	}
	if windowDuration <= 0 {
		windowDuration = defaultWindowDuration
	}
	if now == nil {
		now = time.Now
	}

	nowTime := now()
	return &lineWriter{
		engineName:        engineName,
		stream:            stream,
		logger:            logger,
		maxLinesPerWindow: maxLinesPerWindow,
		windowDuration:    windowDuration,
		now:               now,
		windowStart:       nowTime,
	}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.rotateWindowIfNeededLocked()
	w.buffer = append(w.buffer, p...)

	for {
		lineEnd := bytes.IndexByte(w.buffer, '\n')
		if lineEnd < 0 {
			break
		}

		line := bytes.TrimRight(w.buffer[:lineEnd], "\r")
		w.emitLineLocked(string(line))
		w.buffer = w.buffer[lineEnd+1:]
	}

	if len(w.buffer) > maxBufferedLineBytes {
		w.emitLineLocked(string(w.buffer))
		w.buffer = w.buffer[:0]
	}

	return len(p), nil
}

func (w *lineWriter) emitLineLocked(line string) {
	if line == "" {
		return
	}

	w.rotateWindowIfNeededLocked()
	if w.linesEmitted < w.maxLinesPerWindow {
		w.linesEmitted++
		w.logger.Debug(
			"engine process output",
			"engine", w.engineName,
			"stream", w.stream,
			"line", line,
		)
		return
	}

	w.linesSuppressed++
	if w.linesSuppressed == 1 {
		w.logger.Warn(
			"engine process output suppressed",
			"engine", w.engineName,
			"stream", w.stream,
			"max_lines", w.maxLinesPerWindow,
			"window", w.windowDuration.String(),
		)
	}
}

func (w *lineWriter) rotateWindowIfNeededLocked() {
	now := w.now()
	if now.Sub(w.windowStart) < w.windowDuration {
		return
	}

	if w.linesSuppressed > 0 {
		w.logger.Warn(
			"engine process output suppression summary",
			"engine", w.engineName,
			"stream", w.stream,
			"suppressed_lines", w.linesSuppressed,
			"window", w.windowDuration.String(),
		)
	}

	w.windowStart = now
	w.linesEmitted = 0
	w.linesSuppressed = 0
}

func (w *lineWriter) String() string {
	return fmt.Sprintf("lineWriter(engine=%s, stream=%s)", w.engineName, w.stream)
}
