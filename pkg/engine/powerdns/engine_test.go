package powerdns

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/astradns/astradns-types/engine"
	"github.com/miekg/dns"
)

var _ engine.Engine = (*PowerDNSEngine)(nil)

func TestNewPowerDNSEngine(t *testing.T) {
	e := NewPowerDNSEngine(t.TempDir())
	if e == nil {
		t.Fatal("NewPowerDNSEngine() returned nil")
	}
}

func TestPowerDNSEngineConfigureWritesConfig(t *testing.T) {
	configDir := t.TempDir()
	e := NewPowerDNSEngine(configDir)

	path, err := e.Configure(context.Background(), testEngineConfig("127.0.0.1", 5353))
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	expectedPath := filepath.Join(configDir, "recursor.conf")
	if path != expectedPath {
		t.Fatalf("expected config path %q, got %q", expectedPath, path)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config file at %q: %v", path, err)
	}
}

func TestPowerDNSEngineStartRequiresConfiguration(t *testing.T) {
	e := NewPowerDNSEngine(t.TempDir())

	if err := e.Start(context.Background()); err == nil {
		t.Fatal("expected Start() to fail when engine is not configured")
	}
}

func TestPowerDNSEngineReloadWithCanceledContext(t *testing.T) {
	e := NewPowerDNSEngine(t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := e.Reload(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestPowerDNSEngineStopWithoutProcess(t *testing.T) {
	e := NewPowerDNSEngine(t.TempDir())

	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestPowerDNSEngineHealthCheck(t *testing.T) {
	listenAddr, listenPort := startMockDNSServer(t)

	e := NewPowerDNSEngine(t.TempDir())
	if _, err := e.Configure(context.Background(), testEngineConfig(listenAddr, listenPort)); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	healthy, err := e.HealthCheck(ctx)
	if err != nil {
		t.Fatalf("HealthCheck() error = %v", err)
	}
	if !healthy {
		t.Fatal("expected HealthCheck() to return healthy")
	}
}

func TestPowerDNSEngineName(t *testing.T) {
	e := NewPowerDNSEngine(t.TempDir())
	if got := e.Name(); got != engine.EnginePowerDNS {
		t.Fatalf("expected Name() = %q, got %q", engine.EnginePowerDNS, got)
	}
}

func testEngineConfig(listenAddr string, listenPort int) engine.EngineConfig {
	return engine.EngineConfig{
		Upstreams: []engine.UpstreamConfig{
			{Address: "1.1.1.1", Port: 53},
		},
		Cache: engine.CacheConfig{
			MaxEntries:        1000,
			PositiveTtlMin:    60,
			PositiveTtlMax:    300,
			NegativeTtl:       30,
			PrefetchEnabled:   true,
			PrefetchThreshold: 10,
		},
		ListenAddr: listenAddr,
		ListenPort: listenPort,
	}
}

func startMockDNSServer(t *testing.T) (string, int) {
	t.Helper()

	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket() error = %v", err)
	}

	server := &dns.Server{
		PacketConn: packetConn,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			msg := new(dns.Msg)
			msg.SetReply(r)
			msg.Rcode = dns.RcodeSuccess
			_ = w.WriteMsg(msg)
		}),
	}

	go func() {
		_ = server.ActivateAndServe()
	}()

	t.Cleanup(func() {
		_ = server.Shutdown()
	})

	host, port, err := splitHostPort(packetConn.LocalAddr().String())
	if err != nil {
		t.Fatalf("failed to parse mock DNS address: %v", err)
	}

	waitForDNSReady(t, net.JoinHostPort(host, strconv.Itoa(port)))
	return host, port
}

func splitHostPort(address string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, err
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}

	return host, port, nil
}

func waitForDNSReady(t *testing.T, address string) {
	t.Helper()

	client := &dns.Client{Timeout: 50 * time.Millisecond}
	msg := new(dns.Msg)
	msg.SetQuestion(".", dns.TypeA)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, _, err := client.Exchange(msg, address)
		if err == nil && resp != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("mock DNS server did not become ready at %s", address)
}
