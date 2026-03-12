package coredns

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

// CoreDNSEngine manages the lifecycle of a CoreDNS subprocess.
type CoreDNSEngine struct {
	configDir  string
	configPath string
	config     engine.EngineConfig
	cmd        *exec.Cmd
	mu         sync.Mutex
}

func init() {
	engine.Register(engine.EngineCoreDNS, func(configDir string) engine.Engine {
		return NewCoreDNSEngine(configDir)
	})
}

// NewCoreDNSEngine creates a new CoreDNSEngine.
func NewCoreDNSEngine(configDir string) *CoreDNSEngine {
	return &CoreDNSEngine{configDir: configDir}
}

// Configure renders and writes a Corefile and returns its path.
func (e *CoreDNSEngine) Configure(ctx context.Context, config engine.EngineConfig) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	rendered, err := RenderConfig(config)
	if err != nil {
		return "", fmt.Errorf("render coredns config: %w", err)
	}

	if err := os.MkdirAll(e.configDir, 0o755); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}

	configPath := filepath.Join(e.configDir, "Corefile")
	if err := os.WriteFile(configPath, []byte(rendered), 0o644); err != nil {
		return "", fmt.Errorf("write coredns config: %w", err)
	}

	e.config = normalizeConfig(config)
	e.configPath = configPath

	return configPath, nil
}

// Start launches the CoreDNS subprocess.
func (e *CoreDNSEngine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cmd != nil {
		return errors.New("coredns process is already running")
	}
	if e.configPath == "" {
		return errors.New("coredns is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := exec.Command("coredns", "-conf", e.configPath)
	cmd.Stdout = processlog.NewLineWriter(string(engine.EngineCoreDNS), "stdout")
	cmd.Stderr = processlog.NewLineWriter(string(engine.EngineCoreDNS), "stderr")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start coredns: %w", err)
	}

	e.cmd = cmd
	return nil
}

// Reload is a no-op because CoreDNS auto-reloads using the reload directive.
func (e *CoreDNSEngine) Reload(ctx context.Context) error {
	_ = ctx
	return nil
}

// Stop sends SIGTERM to the CoreDNS subprocess and waits for shutdown.
func (e *CoreDNSEngine) Stop(ctx context.Context) error {
	e.mu.Lock()
	cmd := e.cmd
	e.cmd = nil
	e.mu.Unlock()

	if cmd == nil {
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal coredns process: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	if err := waitForExit(waitCtx, cmd); err != nil && !isExpectedExit(err) {
		return fmt.Errorf("wait for coredns process: %w", err)
	}

	return nil
}

// Capabilities returns feature support advertised by the CoreDNS engine adapter.
func (e *CoreDNSEngine) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{
		SupportsHotReload:         true,
		SupportedTransports:       []engine.UpstreamTransport{engine.UpstreamTransportDNS, engine.UpstreamTransportDoT, engine.UpstreamTransportDoH},
		SupportedDNSSECModes:      []engine.DNSSECMode{engine.DNSSECModeOff},
		SupportsTLSServerName:     true,
		SupportsWeightedUpstreams: true,
		SupportsPriorityUpstreams: true,
	}
}

// HealthStatus sends a DNS A query for "." and returns detailed engine health.
func (e *CoreDNSEngine) HealthStatus(ctx context.Context) (engine.EngineHealthStatus, error) {
	startedAt := time.Now()

	e.mu.Lock()
	listenAddr := e.config.ListenAddr
	listenPort := e.config.ListenPort
	e.mu.Unlock()

	if listenAddr == "" || listenPort == 0 {
		err := errors.New("coredns is not configured")
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

// HealthCheck sends a DNS A query for "." to verify CoreDNS is serving queries.
func (e *CoreDNSEngine) HealthCheck(ctx context.Context) (bool, error) {
	status, err := e.HealthStatus(ctx)
	return status.Healthy, err
}

// Name returns the engine type identifier.
func (e *CoreDNSEngine) Name() engine.EngineType {
	return engine.EngineCoreDNS
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
