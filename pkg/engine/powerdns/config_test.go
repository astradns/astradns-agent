package powerdns

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

	if !strings.Contains(rendered, "forward-zones=.=1.1.1.1:53;8.8.8.8:53;9.9.9.9:53") {
		t.Fatalf("expected semicolon-separated forward-zones list\n%s", rendered)
	}
}

func TestRenderConfigNonStandardPort(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 5353})

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "forward-zones=.=1.1.1.1:5353") {
		t.Fatalf("expected non-standard upstream port in config\n%s", rendered)
	}
}

func TestRenderConfigDaemonDisabled(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "daemon=no") {
		t.Fatalf("expected daemon=no in config\n%s", rendered)
	}
}

func TestRenderConfigMaxCacheEntries(t *testing.T) {
	config := testRenderConfigWithUpstreams(engine.UpstreamConfig{Address: "1.1.1.1", Port: 53})
	config.Cache.MaxEntries = 4242

	rendered, err := RenderConfig(config)
	if err != nil {
		t.Fatalf("RenderConfig() error = %v", err)
	}

	if !strings.Contains(rendered, "max-cache-entries=4242") {
		t.Fatalf("expected max-cache-entries in config\n%s", rendered)
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
