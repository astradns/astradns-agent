package unbound

import (
	"strings"
	"testing"

	"github.com/astradns/astradns-types/engine"
)

func TestRenderConfigIncludesAllUpstreams(t *testing.T) {
	config := testRenderConfigWithUpstreams(
		engine.UpstreamConfig{Address: "1.1.1.1", Port: 53},
		engine.UpstreamConfig{Address: "8.8.8.8", Port: 53},
		engine.UpstreamConfig{Address: "9.9.9.9", Port: 53},
	)

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if got := strings.Count(rendered, "forward-addr:"); got != 3 {
		t.Fatalf("expected 3 forward-addr lines, got %d\n%s", got, rendered)
	}
}

func TestRenderConfigPrefetchEnabled(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.Cache.PrefetchEnabled = true

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "prefetch: yes") {
		t.Fatalf("expected prefetch enabled in config\n%s", rendered)
	}
}

func TestRenderConfigPrefetchDisabled(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.Cache.PrefetchEnabled = false

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "prefetch: no") {
		t.Fatalf("expected prefetch disabled in config\n%s", rendered)
	}
}

func TestRenderConfigNonStandardPort(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 5353})

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "forward-addr: 1.1.1.1@5353") {
		t.Fatalf("expected non-standard upstream port in config\n%s", rendered)
	}
}

func TestRenderConfigDefaultPort53HasNoSuffix(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "forward-addr: 1.1.1.1") {
		t.Fatalf("expected upstream address in config\n%s", rendered)
	}
	if strings.Contains(rendered, "forward-addr: 1.1.1.1@53") {
		t.Fatalf("expected no @53 suffix for default port\n%s", rendered)
	}
}

func TestRenderConfigDoTUpstreamEnablesForwardTLS(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "dns.quad9.net", Transport: engine.UpstreamTransportDoT})

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "forward-tls-upstream: yes") {
		t.Fatalf("expected forward-tls-upstream: yes in config\n%s", rendered)
	}
	if !strings.Contains(rendered, "forward-addr: dns.quad9.net@853") {
		t.Fatalf("expected DoT default port 853 in config\n%s", rendered)
	}
}

func TestRenderConfigDoHUpstreamReturnsError(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "dns.google", Transport: engine.UpstreamTransportDoH})

	if _, err := RenderConfig(config); err == nil {
		t.Fatal("expected error for DoH upstream in unbound engine")
	}
}

func testRenderConfigWithUpstreams(upstreams ...engine.UpstreamConfig) engine.EngineConfig {
	return engine.EngineConfig{
		Upstreams: upstreams,
		Cache: engine.CacheConfig{
			MaxEntries:        1000,
			PositiveTtlMin:    60,
			PositiveTtlMax:    300,
			NegativeTtl:       30,
			PrefetchEnabled:   true,
			PrefetchThreshold: 10,
		},
		ListenAddr: "127.0.0.1",
		ListenPort: 5353,
	}
}
