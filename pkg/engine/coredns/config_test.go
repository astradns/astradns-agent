package coredns

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

	for _, addr := range []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"} {
		if !strings.Contains(rendered, addr) {
			t.Fatalf("expected upstream %q in config\n%s", addr, rendered)
		}
	}
}

func TestRenderConfigPrefetchEnabled(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.Cache.PrefetchEnabled = true

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "prefetch ") {
		t.Fatalf("expected prefetch directive in config\n%s", rendered)
	}
}

func TestRenderConfigPrefetchDisabled(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.Cache.PrefetchEnabled = false

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if strings.Contains(rendered, "prefetch ") {
		t.Fatalf("did not expect prefetch directive in config\n%s", rendered)
	}
}

func TestRenderConfigNonStandardPort(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 5353})

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "1.1.1.1:5353") {
		t.Fatalf("expected non-standard upstream port in config\n%s", rendered)
	}
}

func TestRenderConfigSupportsDoTAndDoH(t *testing.T) {
	config := testRenderConfigWithUpstreams(
		engine.UpstreamConfig{Address: "dns.quad9.net", Transport: engine.UpstreamTransportDoT, TLSServerName: "resolver.example"},
		engine.UpstreamConfig{Address: "dns.google", Transport: engine.UpstreamTransportDoH, TLSServerName: "resolver.example"},
	)

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "tls://dns.quad9.net:853") {
		t.Fatalf("expected DoT upstream target in config\n%s", rendered)
	}
	if !strings.Contains(rendered, "https://dns.google:443") {
		t.Fatalf("expected DoH upstream target in config\n%s", rendered)
	}
}

func TestRenderConfigRejectsDNSSECModes(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.DNSSEC.Mode = engine.DNSSECModeValidate

	if _, err := RenderConfig(config); err == nil {
		t.Fatal("expected DNSSEC mode validation error for coredns engine")
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
