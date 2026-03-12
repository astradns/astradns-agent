package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/fsnotify/fsnotify"
)

const defaultDebounceDuration = 1 * time.Second

// ReloadFunc is called when a config file change is detected.
type ReloadFunc func(ctx context.Context) error

// Option configures ConfigWatcher behavior.
type Option func(*ConfigWatcher)

// WithDebounce overrides the change debounce window.
func WithDebounce(debounce time.Duration) Option {
	return func(w *ConfigWatcher) {
		if debounce > 0 {
			w.debounce = debounce
		}
	}
}

// ConfigWatcher watches a config directory for changes and triggers reloads.
// It is designed to work with Kubernetes ConfigMap mounts, where the kubelet
// creates a new timestamped directory and atomically swaps the ..data symlink.
// For this reason, it watches the directory rather than the file itself.
type ConfigWatcher struct {
	configDir string
	onReload  ReloadFunc
	logger    *slog.Logger
	debounce  time.Duration
}

// New creates a ConfigWatcher that monitors configDir for filesystem events
// and calls onReload after a debounce period when changes are detected.
func New(configDir string, onReload ReloadFunc, logger *slog.Logger, options ...Option) *ConfigWatcher {
	if logger == nil {
		logger = slog.Default()
	}

	watcher := &ConfigWatcher{
		configDir: configDir,
		onReload:  onReload,
		logger:    logger,
		debounce:  defaultDebounceDuration,
	}

	for _, option := range options {
		if option != nil {
			option(watcher)
		}
	}

	return watcher
}

// Run starts watching the config directory and blocks until ctx is cancelled.
// On CREATE or WRITE events, it resets a debounce timer and invokes the reload
// callback once the timer expires. Errors from fsnotify or the reload callback
// are logged but do not stop the watcher.
func (w *ConfigWatcher) Run(ctx context.Context) error {
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}
	defer fsWatcher.Close()

	if err := fsWatcher.Add(w.configDir); err != nil {
		return fmt.Errorf("watch directory %s: %w", w.configDir, err)
	}

	w.logger.Info("config watcher started", "dir", w.configDir)

	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			w.logger.Info("config watcher stopped")
			return nil

		case event, ok := <-fsWatcher.Events:
			if !ok {
				return nil
			}

			if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) &&
				!event.Has(fsnotify.Rename) && !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Chmod) {
				continue
			}

			w.logger.Debug("config change detected", "event", event.Op.String(), "name", event.Name)

			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(w.debounce)
			debounceCh = debounceTimer.C

		case err, ok := <-fsWatcher.Errors:
			if !ok {
				return nil
			}
			w.logger.Error("fsnotify error", "error", err)

		case <-debounceCh:
			debounceCh = nil
			debounceTimer = nil

			w.logger.Info("reloading config after change detected")
			if err := w.onReload(ctx); err != nil {
				w.logger.Error("config reload failed", "error", err)
			}
		}
	}
}
