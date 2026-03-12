package bind

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

	for _, addr := range []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"} {
		if !strings.Contains(rendered, addr) {
			t.Fatalf("expected upstream %s in config\n%s", addr, rendered)
		}
	}
}

func TestRenderConfigNonStandardPort(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 5353})

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "1.1.1.1 port 5353") {
		t.Fatalf("expected non-standard upstream port in config\n%s", rendered)
	}
}

func TestRenderConfigForwardOnly(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "forward only") {
		t.Fatalf("expected forward only in config\n%s", rendered)
	}
	if !strings.Contains(rendered, "forwarders") {
		t.Fatalf("expected forwarders block in config\n%s", rendered)
	}
}

func TestRenderConfigDNSSECOff(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.DNSSEC.Mode = engine.DNSSECModeOff

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "dnssec-validation no") {
		t.Fatalf("expected dnssec-validation no in config\n%s", rendered)
	}
}

func TestRenderConfigDNSSECValidate(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.DNSSEC.Mode = engine.DNSSECModeValidate

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "dnssec-validation auto") {
		t.Fatalf("expected dnssec-validation auto in config\n%s", rendered)
	}
}

func TestRenderConfigDoTUpstreamReturnsError(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "dns.quad9.net", Transport: engine.UpstreamTransportDoT})

	if _, err := RenderConfig(config); err == nil {
		t.Fatal("expected error for DoT upstream in bind engine")
	}
}

func TestRenderConfigDoHUpstreamReturnsError(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "dns.google", Transport: engine.UpstreamTransportDoH})

	if _, err := RenderConfig(config); err == nil {
		t.Fatal("expected error for DoH upstream in bind engine")
	}
}

func TestRenderConfigListenAddress(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.ListenAddr = "10.0.0.1"
	config.ListenPort = 5354

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "listen-on port 5354 { 10.0.0.1; }") {
		t.Fatalf("expected listen address in config\n%s", rendered)
	}
}

func TestRenderConfigMaxCacheTTL(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.Cache.PositiveTtlMax = 600
	config.Cache.NegativeTtl = 60

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "max-cache-ttl 600") {
		t.Fatalf("expected max-cache-ttl 600 in config\n%s", rendered)
	}
	if !strings.Contains(rendered, "max-ncache-ttl 60") {
		t.Fatalf("expected max-ncache-ttl 60 in config\n%s", rendered)
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
