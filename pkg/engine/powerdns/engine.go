package powerdns

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

// PowerDNSEngine manages the lifecycle of a PowerDNS Recursor subprocess.
type PowerDNSEngine struct {
	configDir  string
	configPath string
	config     engine.EngineConfig
	cmd        *exec.Cmd
	mu         sync.Mutex
}

func init() {
	engine.Register(engine.EnginePowerDNS, func(configDir string) engine.Engine {
		return NewPowerDNSEngine(configDir)
	})
}

// NewPowerDNSEngine creates a new PowerDNSEngine.
func NewPowerDNSEngine(configDir string) *PowerDNSEngine {
	return &PowerDNSEngine{configDir: configDir}
}

// Configure renders and writes recursor.conf and returns its path.
func (e *PowerDNSEngine) Configure(ctx context.Context, config engine.EngineConfig) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	rendered, err := RenderConfig(config)
	if err != nil {
		return "", fmt.Errorf("render powerdns config: %w", err)
	}

	if err := os.MkdirAll(e.configDir, 0o755); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}

	configPath := filepath.Join(e.configDir, "recursor.conf")
	if err := os.WriteFile(configPath, []byte(rendered), 0o644); err != nil {
		return "", fmt.Errorf("write powerdns config: %w", err)
	}

	e.config = normalizeConfig(config)
	e.configPath = configPath

	return configPath, nil
}

// Start launches the PowerDNS Recursor subprocess.
func (e *PowerDNSEngine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cmd != nil {
		return errors.New("powerdns process is already running")
	}
	if e.configPath == "" {
		return errors.New("powerdns is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := exec.Command("pdns_recursor", fmt.Sprintf("--config-dir=%s", e.configDir))
	cmd.Stdout = processlog.NewLineWriter(string(engine.EnginePowerDNS), "stdout")
	cmd.Stderr = processlog.NewLineWriter(string(engine.EnginePowerDNS), "stderr")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start powerdns: %w", err)
	}

	e.cmd = cmd
	return nil
}

// Reload triggers a PowerDNS Recursor zone reload.
func (e *PowerDNSEngine) Reload(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "rec_control", "reload-zones")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reload powerdns: %w", err)
	}

	return nil
}

// Stop sends SIGTERM to the PowerDNS subprocess and waits for shutdown.
func (e *PowerDNSEngine) Stop(ctx context.Context) error {
	e.mu.Lock()
	cmd := e.cmd
	e.cmd = nil
	e.mu.Unlock()

	if cmd == nil {
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal powerdns process: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	if err := waitForExit(waitCtx, cmd); err != nil && !isExpectedExit(err) {
		return fmt.Errorf("wait for powerdns process: %w", err)
	}

	return nil
}

// Capabilities returns feature support advertised by the PowerDNS engine adapter.
func (e *PowerDNSEngine) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{
		SupportsHotReload:         true,
		SupportedTransports:       []engine.UpstreamTransport{engine.UpstreamTransportDNS},
		SupportedDNSSECModes:      []engine.DNSSECMode{engine.DNSSECModeOff, engine.DNSSECModeProcess, engine.DNSSECModeValidate},
		SupportsTLSServerName:     false,
		SupportsWeightedUpstreams: true,
		SupportsPriorityUpstreams: true,
	}
}

// HealthStatus sends a DNS A query for "." and returns detailed engine health.
func (e *PowerDNSEngine) HealthStatus(ctx context.Context) (engine.EngineHealthStatus, error) {
	startedAt := time.Now()

	e.mu.Lock()
	listenAddr := e.config.ListenAddr
	listenPort := e.config.ListenPort
	e.mu.Unlock()

	if listenAddr == "" || listenPort == 0 {
		err := errors.New("powerdns is not configured")
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

// HealthCheck sends a DNS A query for "." to verify PowerDNS is serving queries.
func (e *PowerDNSEngine) HealthCheck(ctx context.Context) (bool, error) {
	status, err := e.HealthStatus(ctx)
	return status.Healthy, err
}

// Name returns the engine type identifier.
func (e *PowerDNSEngine) Name() engine.EngineType {
	return engine.EnginePowerDNS
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
