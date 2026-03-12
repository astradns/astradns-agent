package bind

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/astradns/astradns-agent/pkg/engine/processlog"
	"github.com/astradns/astradns-types/engine"
	"github.com/miekg/dns"
)

const shutdownTimeout = 5 * time.Second

// BindEngine manages the lifecycle of an ISC BIND resolver subprocess.
type BindEngine struct {
	configDir  string
	configPath string
	config     engine.EngineConfig
	cmd        *exec.Cmd
	mu         sync.Mutex
}

func init() {
	engine.Register(engine.EngineBIND, func(configDir string) engine.Engine {
		return NewBindEngine(configDir)
	})
}

// NewBindEngine creates a new BindEngine.
func NewBindEngine(configDir string) *BindEngine {
	return &BindEngine{configDir: configDir}
}

// Configure renders and writes named.conf and returns its path.
func (e *BindEngine) Configure(ctx context.Context, config engine.EngineConfig) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	rendered, err := RenderConfig(config)
	if err != nil {
		return "", fmt.Errorf("render bind config: %w", err)
	}

	if err := os.MkdirAll(e.configDir, 0o755); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}

	configPath := filepath.Join(e.configDir, "named.conf")
	if err := os.WriteFile(configPath, []byte(rendered), 0o644); err != nil {
		return "", fmt.Errorf("write bind config: %w", err)
	}

	e.config = normalizeConfig(config)
	e.configPath = configPath

	return configPath, nil
}

// Start launches the BIND named subprocess.
func (e *BindEngine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cmd != nil {
		return errors.New("named process is already running")
	}
	if e.configPath == "" {
		return errors.New("bind is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := exec.Command("named",
		"-f",                    // run in foreground
		"-g",                    // log to stderr
		"-c", e.configPath,      // config file
		"-n", strconv.Itoa(int(e.config.WorkerThreads)), // worker threads
		"-u", "astradns",        // run as user
	)
	cmd.Stdout = processlog.NewLineWriter(string(engine.EngineBIND), "stdout")
	cmd.Stderr = processlog.NewLineWriter(string(engine.EngineBIND), "stderr")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start named: %w", err)
	}

	e.cmd = cmd
	return nil
}

// Reload triggers a BIND config reload using rndc.
func (e *BindEngine) Reload(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "rndc", "reload")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reload bind: %w", err)
	}

	return nil
}

// Stop sends SIGTERM to the named subprocess and waits for shutdown.
func (e *BindEngine) Stop(ctx context.Context) error {
	e.mu.Lock()
	cmd := e.cmd
	e.cmd = nil
	e.mu.Unlock()

	if cmd == nil {
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal named process: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	if err := waitForExit(waitCtx, cmd); err != nil && !isExpectedExit(err) {
		return fmt.Errorf("wait for named process: %w", err)
	}

	return nil
}

// Capabilities returns feature support advertised by the BIND engine adapter.
func (e *BindEngine) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{
		SupportsHotReload:         true,
		SupportedTransports:       []engine.UpstreamTransport{engine.UpstreamTransportDNS},
		SupportedDNSSECModes:      []engine.DNSSECMode{engine.DNSSECModeOff, engine.DNSSECModeProcess, engine.DNSSECModeValidate},
		SupportsTLSServerName:     false,
		SupportsWeightedUpstreams: false,
		SupportsPriorityUpstreams: false,
	}
}

// HealthStatus sends a DNS A query for "." and returns detailed engine health.
func (e *BindEngine) HealthStatus(ctx context.Context) (engine.EngineHealthStatus, error) {
	startedAt := time.Now()

	e.mu.Lock()
	listenAddr := e.config.ListenAddr
	listenPort := e.config.ListenPort
	e.mu.Unlock()

	if listenAddr == "" || listenPort == 0 {
		err := errors.New("bind is not configured")
		return engine.EngineHealthStatus{Healthy: false, Reason: err.Error()}, err
	}

	msg := new(dns.Msg)
	msg.SetQuestion(".", dns.TypeA)

	client := &dns.Client{}
	resp, _, err := client.ExchangeContext(ctx, msg, net.JoinHostPort(listenAddr, strconv.Itoa(int(listenPort))))
	latency := time.Since(startedAt)
	if err != nil {
		return engine.EngineHealthStatus{Healthy: false, Latency: latency, Reason: err.Error()}, err
	}
	if resp == nil {
		err = errors.New("empty DNS response")
		return engine.EngineHealthStatus{Healthy: false, Latency: latency, Reason: err.Error()}, err
	}

	if resp.Rcode != dns.RcodeSuccess {
		reason := fmt.Sprintf("unexpected DNS rcode %s", dns.RcodeToString[resp.Rcode])
		return engine.EngineHealthStatus{Healthy: false, Latency: latency, Reason: reason}, nil
	}

	return engine.EngineHealthStatus{Healthy: true, Latency: latency}, nil
}

// HealthCheck sends a DNS A query for "." to verify BIND is serving queries.
func (e *BindEngine) HealthCheck(ctx context.Context) (bool, error) {
	status, err := e.HealthStatus(ctx)
	return status.Healthy, err
}

// Name returns the engine type identifier.
func (e *BindEngine) Name() engine.EngineType {
	return engine.EngineBIND
}

func waitForExit(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timeout waiting for shutdown: %w", ctx.Err())
	}
}

func isExpectedExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}
