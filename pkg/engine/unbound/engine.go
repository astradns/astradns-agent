package unbound

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

	"github.com/astradns/astradns-types/engine"
	"github.com/miekg/dns"
)

const shutdownTimeout = 5 * time.Second

// UnboundEngine manages the lifecycle of an Unbound DNS resolver subprocess.
type UnboundEngine struct {
	configDir  string
	configPath string
	config     engine.EngineConfig
	cmd        *exec.Cmd
	mu         sync.Mutex
}

func init() {
	engine.Register(engine.EngineUnbound, func(configDir string) engine.Engine {
		return NewUnboundEngine(configDir)
	})
}

// NewUnboundEngine creates a new UnboundEngine.
func NewUnboundEngine(configDir string) *UnboundEngine {
	return &UnboundEngine{configDir: configDir}
}

// Configure renders and writes unbound.conf and returns its path.
func (e *UnboundEngine) Configure(ctx context.Context, config engine.EngineConfig) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	rendered, err := RenderConfig(config)
	if err != nil {
		return "", fmt.Errorf("render unbound config: %w", err)
	}

	if err := os.MkdirAll(e.configDir, 0o755); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}

	configPath := filepath.Join(e.configDir, "unbound.conf")
	if err := os.WriteFile(configPath, []byte(rendered), 0o644); err != nil {
		return "", fmt.Errorf("write unbound config: %w", err)
	}

	e.config = normalizeConfig(config)
	e.configPath = configPath

	return configPath, nil
}

// Start launches the Unbound subprocess.
func (e *UnboundEngine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cmd != nil {
		return errors.New("unbound process is already running")
	}
	if e.configPath == "" {
		return errors.New("unbound is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := exec.Command("unbound", "-d", "-c", e.configPath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start unbound: %w", err)
	}

	e.cmd = cmd
	return nil
}

// Reload reloads Unbound configuration using unbound-control.
func (e *UnboundEngine) Reload(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "unbound-control", "reload")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reload unbound: %w", err)
	}

	return nil
}

// Stop sends SIGTERM to the Unbound subprocess and waits for shutdown.
func (e *UnboundEngine) Stop(ctx context.Context) error {
	e.mu.Lock()
	cmd := e.cmd
	e.cmd = nil
	e.mu.Unlock()

	if cmd == nil {
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal unbound process: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	if err := waitForExit(waitCtx, cmd); err != nil && !isExpectedExit(err) {
		return fmt.Errorf("wait for unbound process: %w", err)
	}

	return nil
}

// HealthCheck sends a DNS A query for "." to verify Unbound is serving queries.
func (e *UnboundEngine) HealthCheck(ctx context.Context) (bool, error) {
	e.mu.Lock()
	listenAddr := e.config.ListenAddr
	listenPort := e.config.ListenPort
	e.mu.Unlock()

	if listenAddr == "" || listenPort == 0 {
		return false, errors.New("unbound is not configured")
	}

	msg := new(dns.Msg)
	msg.SetQuestion(".", dns.TypeA)

	client := &dns.Client{}
	resp, _, err := client.ExchangeContext(ctx, msg, net.JoinHostPort(listenAddr, strconv.Itoa(int(listenPort))))
	if err != nil {
		return false, err
	}
	if resp == nil {
		return false, errors.New("empty DNS response")
	}

	return resp.Rcode == dns.RcodeSuccess, nil
}

// Name returns the engine type identifier.
func (e *UnboundEngine) Name() engine.EngineType {
	return engine.EngineUnbound
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
