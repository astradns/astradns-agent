package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfigChange_TriggersReload(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "engine.json")
	if err := os.WriteFile(configFile, []byte(`{"initial": true}`), 0644); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32

	w := New(dir, func(_ context.Context) error {
		reloadCount.Add(1)
		return nil
	}, slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	w.debounce = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Allow time for the watcher to start.
	time.Sleep(200 * time.Millisecond)

	if err := os.WriteFile(configFile, []byte(`{"updated": true}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce to fire.
	time.Sleep(500 * time.Millisecond)

	count := reloadCount.Load()
	if count != 1 {
		t.Errorf("expected reload count 1, got %d", count)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("unexpected error from Run: %v", err)
	}
}

func TestDebounce_RapidChangesResultInSingleReload(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "engine.json")
	if err := os.WriteFile(configFile, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32

	w := New(dir, func(_ context.Context) error {
		reloadCount.Add(1)
		return nil
	}, slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	w.debounce = 300 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Allow time for the watcher to start.
	time.Sleep(200 * time.Millisecond)

	// Write multiple times in rapid succession, all within the debounce window.
	for i := range 5 {
		if err := os.WriteFile(configFile, []byte(`{"iteration": `+string(rune('0'+i))+`}`), 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for the debounce timer to fire (300ms after last write).
	time.Sleep(600 * time.Millisecond)

	count := reloadCount.Load()
	if count != 1 {
		t.Errorf("expected exactly 1 reload from debounced rapid changes, got %d", count)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("unexpected error from Run: %v", err)
	}
}

func TestContextCancellation_StopsWatcher(t *testing.T) {
	dir := t.TempDir()

	w := New(dir, func(_ context.Context) error {
		t.Error("reload should not be called")
		return nil
	}, slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()

	// Allow time for the watcher to start.
	time.Sleep(200 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error on context cancellation, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop within timeout after context cancellation")
	}
}
