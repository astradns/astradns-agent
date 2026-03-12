package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/astradns/astradns-agent/pkg/diagnostics"
	"github.com/astradns/astradns-agent/pkg/health"
	"github.com/astradns/astradns-types/engine"
	"github.com/miekg/dns"
)

// mockEngine implements engine.Engine for testing the health handler.
type mockEngine struct {
	healthStatusFn func(ctx context.Context) (engine.EngineHealthStatus, error)
}

func (m *mockEngine) Configure(_ context.Context, _ engine.EngineConfig) (string, error) {
	return "", nil
}
func (m *mockEngine) Start(_ context.Context) error           { return nil }
func (m *mockEngine) Reload(_ context.Context) error          { return nil }
func (m *mockEngine) Stop(_ context.Context) error            { return nil }
func (m *mockEngine) Capabilities() engine.EngineCapabilities { return engine.EngineCapabilities{} }
func (m *mockEngine) Name() engine.EngineType                 { return "mock" }
func (m *mockEngine) HealthStatus(ctx context.Context) (engine.EngineHealthStatus, error) {
	return m.healthStatusFn(ctx)
}
func (m *mockEngine) HealthCheck(ctx context.Context) (bool, error) {
	status, err := m.healthStatusFn(ctx)
	return status.Healthy, err
}

type mockDiagnosticsProvider struct {
	snapshot diagnostics.Snapshot
}

func (m *mockDiagnosticsProvider) Snapshot() diagnostics.Snapshot {
	return m.snapshot
}

func TestHealthHandler_EngineHealthyAndUpstreamsHealthy(t *testing.T) {
	eng := &mockEngine{
		healthStatusFn: func(_ context.Context) (engine.EngineHealthStatus, error) {
			return engine.EngineHealthStatus{Healthy: true}, nil
		},
	}

	host, port, stop := startHealthTestUpstream(t)
	defer stop()

	checker := health.NewChecker(health.CheckerConfig{
		Upstreams:        []health.UpstreamTarget{{Address: host, Port: port}},
		IntervalSeconds:  30,
		TimeoutSeconds:   1,
		FailureThreshold: 3,
	}, nil)
	checker.CheckNow(context.Background())

	handler := newHealthHandler(eng, checker, nil)

	t.Run("healthz_returns_200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}
		if rec.Body.String() != "ok" {
			t.Fatalf("expected body %q, got %q", "ok", rec.Body.String())
		}
	})

	t.Run("readyz_returns_200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}
		if rec.Body.String() != "ready" {
			t.Fatalf("expected body %q, got %q", "ready", rec.Body.String())
		}
	})
}

func TestHealthHandler_EngineHealthyNoHealthyUpstreams(t *testing.T) {
	eng := &mockEngine{
		healthStatusFn: func(_ context.Context) (engine.EngineHealthStatus, error) {
			return engine.EngineHealthStatus{Healthy: true}, nil
		},
	}

	// Create checker with no upstreams, so HasHealthyUpstream returns false.
	checker := health.NewChecker(health.CheckerConfig{
		Upstreams:        []health.UpstreamTarget{},
		IntervalSeconds:  30,
		TimeoutSeconds:   1,
		FailureThreshold: 3,
	}, nil)

	handler := newHealthHandler(eng, checker, nil)

	t.Run("healthz_returns_200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}
		if rec.Body.String() != "ok" {
			t.Fatalf("expected body %q, got %q", "ok", rec.Body.String())
		}
	})

	t.Run("readyz_returns_503", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected status 503, got %d", rec.Code)
		}
		if rec.Body.String() != "no healthy upstreams" {
			t.Fatalf("expected body %q, got %q", "no healthy upstreams", rec.Body.String())
		}
	})
}

func TestHealthHandler_EngineUnhealthy(t *testing.T) {
	eng := &mockEngine{
		healthStatusFn: func(_ context.Context) (engine.EngineHealthStatus, error) {
			return engine.EngineHealthStatus{Healthy: false, Reason: "engine not responding"}, fmt.Errorf("engine not responding")
		},
	}

	checker := health.NewChecker(health.CheckerConfig{
		Upstreams:        []health.UpstreamTarget{},
		IntervalSeconds:  30,
		TimeoutSeconds:   1,
		FailureThreshold: 3,
	}, nil)

	handler := newHealthHandler(eng, checker, nil)

	t.Run("healthz_returns_503", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected status 503, got %d", rec.Code)
		}
		if rec.Body.String() != "unhealthy" {
			t.Fatalf("expected body %q, got %q", "unhealthy", rec.Body.String())
		}
	})

	t.Run("readyz_returns_503", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected status 503, got %d", rec.Code)
		}
		if rec.Body.String() != "engine unhealthy" {
			t.Fatalf("expected body %q, got %q", "engine unhealthy", rec.Body.String())
		}
	})
}

func TestHealthHandler_EngineHealthCheckReturnsFalseWithoutError(t *testing.T) {
	eng := &mockEngine{
		healthStatusFn: func(_ context.Context) (engine.EngineHealthStatus, error) {
			return engine.EngineHealthStatus{Healthy: false, Reason: "not ready"}, nil
		},
	}

	checker := health.NewChecker(health.CheckerConfig{
		Upstreams:        []health.UpstreamTarget{},
		IntervalSeconds:  30,
		TimeoutSeconds:   1,
		FailureThreshold: 3,
	}, nil)

	handler := newHealthHandler(eng, checker, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 when HealthCheck returns false, got %d", rec.Code)
	}
}

func TestHealthHandler_DiagnosticsDisabled(t *testing.T) {
	eng := &mockEngine{
		healthStatusFn: func(_ context.Context) (engine.EngineHealthStatus, error) {
			return engine.EngineHealthStatus{Healthy: true}, nil
		},
	}

	handler := newHealthHandler(eng, health.NewChecker(health.CheckerConfig{}, nil), nil)
	req := httptest.NewRequest(http.MethodGet, "/diagnostics", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var body struct {
		Enabled bool   `json:"enabled"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode diagnostics response: %v", err)
	}
	if body.Enabled {
		t.Fatal("expected diagnostics endpoint to be disabled")
	}
}

func TestHealthHandler_DiagnosticsEnabled(t *testing.T) {
	eng := &mockEngine{
		healthStatusFn: func(_ context.Context) (engine.EngineHealthStatus, error) {
			return engine.EngineHealthStatus{Healthy: true}, nil
		},
	}

	provider := &mockDiagnosticsProvider{snapshot: diagnostics.Snapshot{Results: []diagnostics.Result{{
		Target:    "s3.us-west-004.backblazeb2.com",
		Diagnosis: diagnostics.DiagnosisEgressBlockedOrNetwork,
	}}}}

	handler := newHealthHandler(eng, health.NewChecker(health.CheckerConfig{}, nil), provider)
	req := httptest.NewRequest(http.MethodGet, "/diagnostics", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var body struct {
		Enabled  bool                 `json:"enabled"`
		Snapshot diagnostics.Snapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode diagnostics response: %v", err)
	}
	if !body.Enabled {
		t.Fatal("expected diagnostics endpoint to be enabled")
	}
	if len(body.Snapshot.Results) != 1 {
		t.Fatalf("expected one diagnostics result, got %d", len(body.Snapshot.Results))
	}
	if body.Snapshot.Results[0].Diagnosis != diagnostics.DiagnosisEgressBlockedOrNetwork {
		t.Fatalf("unexpected diagnosis %q", body.Snapshot.Results[0].Diagnosis)
	}
}

// startHealthTestUpstream starts a responsive UDP DNS server for health handler tests.
func startHealthTestUpstream(t *testing.T) (string, int, func()) {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test upstream: %v", err)
	}

	addr := conn.LocalAddr().(*net.UDPAddr)
	var responsive atomic.Bool
	responsive.Store(true)

	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		if !responsive.Load() {
			return
		}
		response := new(dns.Msg)
		response.SetReply(r)
		_ = w.WriteMsg(response)
	})

	server := &dns.Server{PacketConn: conn, Net: "udp", Handler: handler}
	go func() {
		_ = server.ActivateAndServe()
	}()

	return addr.IP.String(), addr.Port, func() {
		_ = server.Shutdown()
	}
}
