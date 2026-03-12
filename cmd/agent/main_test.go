package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astradns/astradns-agent/pkg/metrics"
	"github.com/astradns/astradns-agent/pkg/proxy"
	"github.com/astradns/astradns-types/engine"
	"github.com/miekg/dns"
)

func TestFanOutReportsDroppedEvents(t *testing.T) {
	in := make(chan proxy.QueryEvent, 1)
	outBuffered := make(chan proxy.QueryEvent, 1)
	outDropped := make(chan proxy.QueryEvent)

	in <- proxy.QueryEvent{QueryType: "A"}
	close(in)

	var dropped atomic.Int64
	fanOut(in, func() {
		dropped.Add(1)
	}, outBuffered, outDropped)

	if dropped.Load() != 1 {
		t.Fatalf("expected one dropped event, got %d", dropped.Load())
	}

	if _, ok := <-outBuffered; !ok {
		t.Fatal("expected buffered output to receive event before close")
	}

	if _, ok := <-outBuffered; ok {
		t.Fatal("expected buffered output channel to be closed")
	}

	if _, ok := <-outDropped; ok {
		t.Fatal("expected dropped output channel to be closed")
	}
}

// --- Gap 2: loadEngineConfig tests ---

func TestLoadEngineConfig_FileDoesNotExist(t *testing.T) {
	nonExistent := filepath.Join(t.TempDir(), "does_not_exist.json")
	engineAddr := "127.0.0.1:5354"

	cfg, err := loadEngineConfig(nonExistent, engineAddr)
	if err != nil {
		t.Fatalf("expected no error for missing config, got: %v", err)
	}

	defaults := defaultEngineConfig(engineAddr)
	if cfg.ListenAddr != defaults.ListenAddr {
		t.Fatalf("expected default ListenAddr %q, got %q", defaults.ListenAddr, cfg.ListenAddr)
	}
	if cfg.ListenPort != defaults.ListenPort {
		t.Fatalf("expected default ListenPort %d, got %d", defaults.ListenPort, cfg.ListenPort)
	}
	if len(cfg.Upstreams) != len(defaults.Upstreams) {
		t.Fatalf("expected %d default upstreams, got %d", len(defaults.Upstreams), len(cfg.Upstreams))
	}
	if cfg.Cache.MaxEntries != defaults.Cache.MaxEntries {
		t.Fatalf("expected default cache MaxEntries %d, got %d", defaults.Cache.MaxEntries, cfg.Cache.MaxEntries)
	}
}

func TestLoadEngineConfig_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")

	cfg := engine.EngineConfig{
		Upstreams: []engine.UpstreamConfig{
			{Address: "8.8.8.8", Port: 53},
			{Address: "8.8.4.4", Port: 53},
		},
		Cache: engine.CacheConfig{
			MaxEntries:        5000,
			PositiveTtlMin:    60,
			PositiveTtlMax:    600,
			NegativeTtl:       60,
			PrefetchEnabled:   false,
			PrefetchThreshold: 5,
		},
		ListenAddr: "0.0.0.0",
		ListenPort: 5300,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}
	if err := os.WriteFile(configFile, data, 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	loaded, err := loadEngineConfig(configFile, "127.0.0.1:5354")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(loaded.Upstreams) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(loaded.Upstreams))
	}
	if loaded.Upstreams[0].Address != "8.8.8.8" {
		t.Fatalf("expected upstream address 8.8.8.8, got %q", loaded.Upstreams[0].Address)
	}
	if loaded.Upstreams[1].Address != "8.8.4.4" {
		t.Fatalf("expected upstream address 8.8.4.4, got %q", loaded.Upstreams[1].Address)
	}
	if loaded.Cache.MaxEntries != 5000 {
		t.Fatalf("expected cache MaxEntries 5000, got %d", loaded.Cache.MaxEntries)
	}
	if loaded.ListenAddr != "0.0.0.0" {
		t.Fatalf("expected ListenAddr 0.0.0.0, got %q", loaded.ListenAddr)
	}
	if loaded.ListenPort != 5300 {
		t.Fatalf("expected ListenPort 5300, got %d", loaded.ListenPort)
	}
}

func TestLoadEngineConfig_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")

	if err := os.WriteFile(configFile, []byte(`{invalid json`), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	_, err := loadEngineConfig(configFile, "127.0.0.1:5354")
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestLoadEngineConfig_PartialConfig_OnlyUpstreams(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")

	// Only upstreams specified (port omitted), no cache or listen fields.
	partial := `{"upstreams": [{"address": "9.9.9.9"}]}`
	if err := os.WriteFile(configFile, []byte(partial), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	engineAddr := "127.0.0.1:5354"
	loaded, err := loadEngineConfig(configFile, engineAddr)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	defaults := defaultEngineConfig(engineAddr)

	// Upstreams should be from config, not defaults.
	if len(loaded.Upstreams) != 1 {
		t.Fatalf("expected 1 upstream from config, got %d", len(loaded.Upstreams))
	}
	if loaded.Upstreams[0].Address != "9.9.9.9" {
		t.Fatalf("expected upstream address 9.9.9.9, got %q", loaded.Upstreams[0].Address)
	}
	if loaded.Upstreams[0].Port != defaults.Upstreams[0].Port {
		t.Fatalf("expected default upstream port %d, got %d", defaults.Upstreams[0].Port, loaded.Upstreams[0].Port)
	}

	// Cache should fall back to defaults since MaxEntries == 0.
	if loaded.Cache.MaxEntries != defaults.Cache.MaxEntries {
		t.Fatalf("expected default cache MaxEntries %d, got %d", defaults.Cache.MaxEntries, loaded.Cache.MaxEntries)
	}

	// ListenAddr and ListenPort should fall back to defaults.
	if loaded.ListenAddr != defaults.ListenAddr {
		t.Fatalf("expected default ListenAddr %q, got %q", defaults.ListenAddr, loaded.ListenAddr)
	}
	if loaded.ListenPort != defaults.ListenPort {
		t.Fatalf("expected default ListenPort %d, got %d", defaults.ListenPort, loaded.ListenPort)
	}
}

func TestLoadEngineConfig_EmptyJSON(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")

	if err := os.WriteFile(configFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	engineAddr := "127.0.0.1:5354"
	loaded, err := loadEngineConfig(configFile, engineAddr)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	defaults := defaultEngineConfig(engineAddr)

	if loaded.ListenAddr != defaults.ListenAddr {
		t.Fatalf("expected default ListenAddr %q, got %q", defaults.ListenAddr, loaded.ListenAddr)
	}
	if loaded.ListenPort != defaults.ListenPort {
		t.Fatalf("expected default ListenPort %d, got %d", defaults.ListenPort, loaded.ListenPort)
	}
	if len(loaded.Upstreams) != len(defaults.Upstreams) {
		t.Fatalf("expected %d default upstreams, got %d", len(defaults.Upstreams), len(loaded.Upstreams))
	}
	if loaded.Cache.MaxEntries != defaults.Cache.MaxEntries {
		t.Fatalf("expected default cache MaxEntries %d, got %d", defaults.Cache.MaxEntries, loaded.Cache.MaxEntries)
	}
}

func TestLoadEngineConfig_EmptyListenAddr_FallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")

	// Provide cache and upstreams but leave listenAddr empty.
	cfg := `{
		"upstreams": [{"address": "1.1.1.1", "port": 53}],
		"cache": {"maxEntries": 2000},
		"listenAddr": "",
		"listenPort": 5354
	}`
	if err := os.WriteFile(configFile, []byte(cfg), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	engineAddr := "127.0.0.1:5354"
	loaded, err := loadEngineConfig(configFile, engineAddr)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	defaults := defaultEngineConfig(engineAddr)
	if loaded.ListenAddr != defaults.ListenAddr {
		t.Fatalf("expected default ListenAddr %q when empty, got %q", defaults.ListenAddr, loaded.ListenAddr)
	}
}

func TestLoadEngineConfig_PartialCacheFieldsDefaulted(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")

	cfg := `{
		"upstreams": [{"address": "1.1.1.1", "port": 53}],
		"cache": {"maxEntries": 2500},
		"listenAddr": "127.0.0.1",
		"listenPort": 5354
	}`
	if err := os.WriteFile(configFile, []byte(cfg), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	engineAddr := "127.0.0.1:5354"
	loaded, err := loadEngineConfig(configFile, engineAddr)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	defaults := defaultEngineConfig(engineAddr)
	if loaded.Cache.MaxEntries != 2500 {
		t.Fatalf("expected cache MaxEntries 2500, got %d", loaded.Cache.MaxEntries)
	}
	if loaded.Cache.PositiveTtlMin != defaults.Cache.PositiveTtlMin {
		t.Fatalf(
			"expected default cache PositiveTtlMin %d, got %d",
			defaults.Cache.PositiveTtlMin,
			loaded.Cache.PositiveTtlMin,
		)
	}
	if loaded.Cache.PositiveTtlMax != defaults.Cache.PositiveTtlMax {
		t.Fatalf(
			"expected default cache PositiveTtlMax %d, got %d",
			defaults.Cache.PositiveTtlMax,
			loaded.Cache.PositiveTtlMax,
		)
	}
	if loaded.Cache.NegativeTtl != defaults.Cache.NegativeTtl {
		t.Fatalf(
			"expected default cache NegativeTtl %d, got %d",
			defaults.Cache.NegativeTtl,
			loaded.Cache.NegativeTtl,
		)
	}
	if loaded.Cache.PrefetchThreshold != defaults.Cache.PrefetchThreshold {
		t.Fatalf(
			"expected default cache PrefetchThreshold %d, got %d",
			defaults.Cache.PrefetchThreshold,
			loaded.Cache.PrefetchThreshold,
		)
	}
}

// --- Gap 4: splitHostPort, defaultEngineConfig, resolveConfigPaths tests ---

func TestSplitHostPort(t *testing.T) {
	tests := []struct {
		name         string
		addr         string
		defaultHost  string
		defaultPort  int
		expectedHost string
		expectedPort int
	}{
		{
			name:         "valid host:port",
			addr:         "10.0.0.1:8053",
			defaultHost:  "127.0.0.1",
			defaultPort:  5354,
			expectedHost: "10.0.0.1",
			expectedPort: 8053,
		},
		{
			name:         "host only without port falls back to defaults",
			addr:         "10.0.0.1",
			defaultHost:  "127.0.0.1",
			defaultPort:  5354,
			expectedHost: "127.0.0.1",
			expectedPort: 5354,
		},
		{
			name:         "empty string falls back to defaults",
			addr:         "",
			defaultHost:  "127.0.0.1",
			defaultPort:  5354,
			expectedHost: "127.0.0.1",
			expectedPort: 5354,
		},
		{
			name:         "empty host in host:port uses default host",
			addr:         ":5354",
			defaultHost:  "127.0.0.1",
			defaultPort:  5354,
			expectedHost: "127.0.0.1",
			expectedPort: 5354,
		},
		{
			name:         "invalid port falls back to defaults",
			addr:         "10.0.0.1:abc",
			defaultHost:  "127.0.0.1",
			defaultPort:  5354,
			expectedHost: "127.0.0.1",
			expectedPort: 5354,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port := splitHostPort(tt.addr, tt.defaultHost, tt.defaultPort)
			if host != tt.expectedHost {
				t.Fatalf("expected host %q, got %q", tt.expectedHost, host)
			}
			if port != tt.expectedPort {
				t.Fatalf("expected port %d, got %d", tt.expectedPort, port)
			}
		})
	}
}

func TestDefaultEngineConfig(t *testing.T) {
	cfg := defaultEngineConfig("192.168.1.1:5354")

	if cfg.ListenAddr != "192.168.1.1" {
		t.Fatalf("expected ListenAddr 192.168.1.1, got %q", cfg.ListenAddr)
	}
	if cfg.ListenPort != 5354 {
		t.Fatalf("expected ListenPort 5354, got %d", cfg.ListenPort)
	}
	if len(cfg.Upstreams) != 1 {
		t.Fatalf("expected 1 default upstream, got %d", len(cfg.Upstreams))
	}
	if cfg.Upstreams[0].Address != "1.1.1.1" {
		t.Fatalf("expected default upstream 1.1.1.1, got %q", cfg.Upstreams[0].Address)
	}
	if cfg.Upstreams[0].Port != 53 {
		t.Fatalf("expected default upstream port 53, got %d", cfg.Upstreams[0].Port)
	}
	if cfg.Upstreams[0].Transport != engine.UpstreamTransportDNS {
		t.Fatalf("expected default upstream transport dns, got %q", cfg.Upstreams[0].Transport)
	}
	if cfg.WorkerThreads <= 0 {
		t.Fatalf("expected positive default worker threads, got %d", cfg.WorkerThreads)
	}
	if cfg.DNSSEC.Mode != engine.DNSSECModeOff {
		t.Fatalf("expected default DNSSEC mode off, got %q", cfg.DNSSEC.Mode)
	}
	if cfg.Cache.MaxEntries != 10000 {
		t.Fatalf("expected cache MaxEntries 10000, got %d", cfg.Cache.MaxEntries)
	}
	if !cfg.Cache.PrefetchEnabled {
		t.Fatal("expected PrefetchEnabled to be true by default")
	}
}

func TestDefaultEngineConfig_InvalidAddr(t *testing.T) {
	cfg := defaultEngineConfig("not-a-valid-addr")

	if cfg.ListenAddr != "127.0.0.1" {
		t.Fatalf("expected fallback ListenAddr 127.0.0.1, got %q", cfg.ListenAddr)
	}
	if cfg.ListenPort != 5354 {
		t.Fatalf("expected fallback ListenPort 5354, got %d", cfg.ListenPort)
	}
}

func TestResolveConfigPaths_DirectoryPath(t *testing.T) {
	dir := t.TempDir()

	sourceDir, configFile := resolveConfigPaths(dir)
	if sourceDir != dir {
		t.Fatalf("expected sourceDir %q, got %q", dir, sourceDir)
	}
	expected := filepath.Join(dir, defaultConfigFile)
	if configFile != expected {
		t.Fatalf("expected configFile %q, got %q", expected, configFile)
	}
}

func TestResolveConfigPaths_JSONFilePath(t *testing.T) {
	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "myconfig.json")
	if err := os.WriteFile(jsonFile, []byte(`{}`), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	sourceDir, configFile := resolveConfigPaths(jsonFile)
	if sourceDir != dir {
		t.Fatalf("expected sourceDir %q, got %q", dir, sourceDir)
	}
	if configFile != jsonFile {
		t.Fatalf("expected configFile %q, got %q", jsonFile, configFile)
	}
}

func TestResolveConfigPaths_NonExistentJSONFile(t *testing.T) {
	// A path that doesn't exist but has .json extension.
	path := "/tmp/nonexistent_astradns_test/custom.json"

	sourceDir, configFile := resolveConfigPaths(path)
	if sourceDir != filepath.Dir(path) {
		t.Fatalf("expected sourceDir %q, got %q", filepath.Dir(path), sourceDir)
	}
	if configFile != path {
		t.Fatalf("expected configFile %q, got %q", path, configFile)
	}
}

func TestResolveConfigPaths_NonExistentNonJSONPath(t *testing.T) {
	// A path that doesn't exist and has no .json extension (treated as a directory).
	path := "/tmp/nonexistent_astradns_test/configs"

	sourceDir, configFile := resolveConfigPaths(path)
	if sourceDir != path {
		t.Fatalf("expected sourceDir %q, got %q", path, sourceDir)
	}
	expected := filepath.Join(path, defaultConfigFile)
	if configFile != expected {
		t.Fatalf("expected configFile %q, got %q", expected, configFile)
	}
}

func TestGetEnvDuration(t *testing.T) {
	t.Setenv("ASTRADNS_TEST_DURATION", "3s")
	if got := getEnvDuration("ASTRADNS_TEST_DURATION", time.Second); got != 3*time.Second {
		t.Fatalf("expected 3s duration, got %v", got)
	}

	t.Setenv("ASTRADNS_TEST_DURATION", "not-a-duration")
	if got := getEnvDuration("ASTRADNS_TEST_DURATION", time.Second); got != time.Second {
		t.Fatalf("expected fallback duration, got %v", got)
	}

	t.Setenv("ASTRADNS_TEST_DURATION", "-2s")
	if got := getEnvDuration("ASTRADNS_TEST_DURATION", time.Second); got != time.Second {
		t.Fatalf("expected fallback for negative duration, got %v", got)
	}
}

func TestGetEnvDNSQueryType(t *testing.T) {
	t.Setenv("ASTRADNS_TEST_QUERY_TYPE", "aaaa")
	if got := getEnvDNSQueryType("ASTRADNS_TEST_QUERY_TYPE", dns.TypeA); got != dns.TypeAAAA {
		t.Fatalf("expected AAAA type, got %d", got)
	}

	t.Setenv("ASTRADNS_TEST_QUERY_TYPE", "15")
	if got := getEnvDNSQueryType("ASTRADNS_TEST_QUERY_TYPE", dns.TypeA); got != dns.TypeMX {
		t.Fatalf("expected MX type from numeric value, got %d", got)
	}

	t.Setenv("ASTRADNS_TEST_QUERY_TYPE", "0")
	if got := getEnvDNSQueryType("ASTRADNS_TEST_QUERY_TYPE", dns.TypeA); got != dns.TypeA {
		t.Fatalf("expected fallback for zero type, got %d", got)
	}

	t.Setenv("ASTRADNS_TEST_QUERY_TYPE", "not-a-type")
	if got := getEnvDNSQueryType("ASTRADNS_TEST_QUERY_TYPE", dns.TypeA); got != dns.TypeA {
		t.Fatalf("expected fallback for invalid type, got %d", got)
	}
}

func TestLoadRuntimeConfigHealthProbeOverrides(t *testing.T) {
	t.Setenv("ASTRADNS_HEALTH_PROBE_DOMAIN", "example.org")
	t.Setenv("ASTRADNS_HEALTH_PROBE_TYPE", "TXT")

	cfg := loadRuntimeConfig()
	if cfg.HealthProbeDomain != "example.org" {
		t.Fatalf("expected health probe domain override, got %q", cfg.HealthProbeDomain)
	}
	if cfg.HealthProbeType != dns.TypeTXT {
		t.Fatalf("expected health probe type TXT (%d), got %d", dns.TypeTXT, cfg.HealthProbeType)
	}
}

func TestLoadRuntimeConfigProxyAndTracingOverrides(t *testing.T) {
	t.Setenv("ASTRADNS_PROXY_CACHE_MAX_ENTRIES", "2048")
	t.Setenv("ASTRADNS_PROXY_CACHE_DEFAULT_TTL", "45s")
	t.Setenv("ASTRADNS_ENGINE_CONN_POOL_SIZE", "32")
	t.Setenv("ASTRADNS_METRICS_BEARER_TOKEN", "secret")
	t.Setenv("ASTRADNS_DIAGNOSTICS_ENABLED", "true")
	t.Setenv("ASTRADNS_DIAGNOSTICS_TARGETS", "s3.us-west-004.backblazeb2.com, dns.google")
	t.Setenv("ASTRADNS_DIAGNOSTICS_INTERVAL", "2m")
	t.Setenv("ASTRADNS_DIAGNOSTICS_TIMEOUT", "4s")
	t.Setenv("ASTRADNS_TRACING_ENABLED", "true")
	t.Setenv("ASTRADNS_TRACING_ENDPOINT", "otel-collector.monitoring.svc:4318")
	t.Setenv("ASTRADNS_TRACING_INSECURE", "false")
	t.Setenv("ASTRADNS_TRACING_SAMPLE_RATIO", "0.25")
	t.Setenv("ASTRADNS_TRACING_SERVICE_NAME", "astradns-agent-test")

	cfg := loadRuntimeConfig()
	if cfg.ProxyCacheMaxEntries != 2048 {
		t.Fatalf("expected proxy cache max entries 2048, got %d", cfg.ProxyCacheMaxEntries)
	}
	if cfg.ProxyCacheDefaultTTL != 45*time.Second {
		t.Fatalf("expected proxy cache ttl 45s, got %s", cfg.ProxyCacheDefaultTTL)
	}
	if cfg.EngineConnectionPoolSize != 32 {
		t.Fatalf("expected engine connection pool size 32, got %d", cfg.EngineConnectionPoolSize)
	}
	if cfg.MetricsBearerToken != "secret" {
		t.Fatalf("expected metrics bearer token to be loaded")
	}
	if !cfg.DiagnosticsEnabled {
		t.Fatal("expected diagnostics to be enabled")
	}
	if len(cfg.DiagnosticsTargets) != 2 {
		t.Fatalf("expected 2 diagnostics targets, got %d", len(cfg.DiagnosticsTargets))
	}
	if cfg.DiagnosticsInterval != 2*time.Minute {
		t.Fatalf("expected diagnostics interval 2m, got %s", cfg.DiagnosticsInterval)
	}
	if cfg.DiagnosticsTimeout != 4*time.Second {
		t.Fatalf("expected diagnostics timeout 4s, got %s", cfg.DiagnosticsTimeout)
	}
	if !cfg.TracingEnabled {
		t.Fatal("expected tracing to be enabled")
	}
	if cfg.TracingEndpoint != "otel-collector.monitoring.svc:4318" {
		t.Fatalf("unexpected tracing endpoint %q", cfg.TracingEndpoint)
	}
	if cfg.TracingInsecure {
		t.Fatal("expected tracing insecure to be false")
	}
	if cfg.TracingSampleRatio != 0.25 {
		t.Fatalf("expected tracing sample ratio 0.25, got %v", cfg.TracingSampleRatio)
	}
	if cfg.TracingServiceName != "astradns-agent-test" {
		t.Fatalf("unexpected tracing service name %q", cfg.TracingServiceName)
	}
}

func TestParseDiagnosticsTargets(t *testing.T) {
	targets := parseDiagnosticsTargets(" s3.us-west-004.backblazeb2.com,invalid target,,DNS.GOOGLE.,1.1.1.1,s3.us-west-004.backblazeb2.com")
	if len(targets) != 3 {
		t.Fatalf("expected 3 valid diagnostics targets, got %d (%v)", len(targets), targets)
	}
	if targets[0] != "s3.us-west-004.backblazeb2.com" {
		t.Fatalf("unexpected first target: %q", targets[0])
	}
	if targets[1] != "dns.google" {
		t.Fatalf("unexpected second target: %q", targets[1])
	}
	if targets[2] != "1.1.1.1" {
		t.Fatalf("unexpected third target: %q", targets[2])
	}
}

func TestDiagnosticsResolverAddress(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		output string
	}{
		{name: "wildcard ipv4", input: "0.0.0.0:5353", output: "127.0.0.1:5353"},
		{name: "explicit host", input: "169.254.20.11:5353", output: "169.254.20.11:5353"},
		{name: "invalid value", input: "invalid", output: "127.0.0.1:5353"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diagnosticsResolverAddress(tt.input); got != tt.output {
				t.Fatalf("expected resolver address %q, got %q", tt.output, got)
			}
		})
	}
}

func TestGetEnvFloat(t *testing.T) {
	t.Setenv("ASTRADNS_TEST_FLOAT", "0.25")
	if got := getEnvFloat("ASTRADNS_TEST_FLOAT", 0.1); got != 0.25 {
		t.Fatalf("expected 0.25, got %v", got)
	}

	t.Setenv("ASTRADNS_TEST_FLOAT", "not-a-float")
	if got := getEnvFloat("ASTRADNS_TEST_FLOAT", 0.1); got != 0.1 {
		t.Fatalf("expected fallback 0.1 for parse error, got %v", got)
	}

	t.Setenv("ASTRADNS_TEST_FLOAT", "NaN")
	if got := getEnvFloat("ASTRADNS_TEST_FLOAT", 0.1); got != 0.1 {
		t.Fatalf("expected fallback 0.1 for NaN, got %v", got)
	}

	t.Setenv("ASTRADNS_TEST_FLOAT", "+Inf")
	if got := getEnvFloat("ASTRADNS_TEST_FLOAT", 0.1); got != 0.1 {
		t.Fatalf("expected fallback 0.1 for +Inf, got %v", got)
	}
}

func TestGetEnvPositiveFloat(t *testing.T) {
	t.Setenv("ASTRADNS_TEST_POS_FLOAT", "2.5")
	if got := getEnvPositiveFloat("ASTRADNS_TEST_POS_FLOAT", 1.0); got != 2.5 {
		t.Fatalf("expected 2.5, got %v", got)
	}

	t.Setenv("ASTRADNS_TEST_POS_FLOAT", "0")
	if got := getEnvPositiveFloat("ASTRADNS_TEST_POS_FLOAT", 1.0); got != 1.0 {
		t.Fatalf("expected fallback 1.0 for zero, got %v", got)
	}

	t.Setenv("ASTRADNS_TEST_POS_FLOAT", "-3")
	if got := getEnvPositiveFloat("ASTRADNS_TEST_POS_FLOAT", 1.0); got != 1.0 {
		t.Fatalf("expected fallback 1.0 for negative, got %v", got)
	}

	t.Setenv("ASTRADNS_TEST_POS_FLOAT", "NaN")
	if got := getEnvPositiveFloat("ASTRADNS_TEST_POS_FLOAT", 1.0); got != 1.0 {
		t.Fatalf("expected fallback 1.0 for NaN, got %v", got)
	}
}

func TestGetEnvPositiveInt(t *testing.T) {
	t.Setenv("ASTRADNS_TEST_POS_INT", "42")
	if got := getEnvPositiveInt("ASTRADNS_TEST_POS_INT", 10); got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}

	t.Setenv("ASTRADNS_TEST_POS_INT", "0")
	if got := getEnvPositiveInt("ASTRADNS_TEST_POS_INT", 10); got != 10 {
		t.Fatalf("expected fallback 10 for zero, got %d", got)
	}

	t.Setenv("ASTRADNS_TEST_POS_INT", "-5")
	if got := getEnvPositiveInt("ASTRADNS_TEST_POS_INT", 10); got != 10 {
		t.Fatalf("expected fallback 10 for negative, got %d", got)
	}

	t.Setenv("ASTRADNS_TEST_POS_INT", "not-an-int")
	if got := getEnvPositiveInt("ASTRADNS_TEST_POS_INT", 10); got != 10 {
		t.Fatalf("expected fallback 10 for invalid value, got %d", got)
	}
}

func TestGetEnvNonNegativeInt(t *testing.T) {
	t.Setenv("ASTRADNS_TEST_NON_NEG_INT", "0")
	if got := getEnvNonNegativeInt("ASTRADNS_TEST_NON_NEG_INT", 5); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}

	t.Setenv("ASTRADNS_TEST_NON_NEG_INT", "12")
	if got := getEnvNonNegativeInt("ASTRADNS_TEST_NON_NEG_INT", 5); got != 12 {
		t.Fatalf("expected 12, got %d", got)
	}

	t.Setenv("ASTRADNS_TEST_NON_NEG_INT", "-2")
	if got := getEnvNonNegativeInt("ASTRADNS_TEST_NON_NEG_INT", 5); got != 5 {
		t.Fatalf("expected fallback 5 for negative value, got %d", got)
	}

	t.Setenv("ASTRADNS_TEST_NON_NEG_INT", "invalid")
	if got := getEnvNonNegativeInt("ASTRADNS_TEST_NON_NEG_INT", 5); got != 5 {
		t.Fatalf("expected fallback 5 for invalid value, got %d", got)
	}
}

func TestGetEnvBool(t *testing.T) {
	t.Setenv("ASTRADNS_TEST_BOOL", "true")
	if got := getEnvBool("ASTRADNS_TEST_BOOL", false); !got {
		t.Fatal("expected true value")
	}

	t.Setenv("ASTRADNS_TEST_BOOL", "0")
	if got := getEnvBool("ASTRADNS_TEST_BOOL", true); got {
		t.Fatal("expected false value from numeric false")
	}

	t.Setenv("ASTRADNS_TEST_BOOL", "invalid")
	if got := getEnvBool("ASTRADNS_TEST_BOOL", true); !got {
		t.Fatal("expected fallback true for invalid bool")
	}
}

func TestNewMetricsHandlerRejectsUnauthorizedRequests(t *testing.T) {
	handler := newMetricsHandler(metrics.NewCollector(nil), "top-secret")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", rec.Code)
	}
}

func TestNewMetricsHandlerAcceptsAuthorizedRequests(t *testing.T) {
	handler := newMetricsHandler(metrics.NewCollector(nil), "top-secret")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer top-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
}

func TestNewMetricsHandlerWithoutTokenKeepsEndpointOpen(t *testing.T) {
	handler := newMetricsHandler(metrics.NewCollector(nil), "")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
}

func TestValidateEngineConfig(t *testing.T) {
	valid := defaultEngineConfig("127.0.0.1:5354")
	if err := validateEngineConfig(valid); err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}

	invalid := valid
	invalid.ListenAddr = "   "
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for whitespace listenAddr")
	}

	invalid = valid
	invalid.Upstreams = nil
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for missing upstreams")
	}

	invalid = valid
	invalid.Upstreams[0].Address = "   "
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for whitespace upstream address")
	}

	invalid = valid
	invalid.Upstreams[0].Address = "999.999.999.999"
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for invalid IPv4 literal")
	}

	invalid = valid
	invalid.Upstreams[0].Address = "-bad.host"
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for invalid DNS hostname")
	}

	invalid = valid
	invalid.Cache.PositiveTtlMin = 100
	invalid.Cache.PositiveTtlMax = 10
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for invalid ttl bounds")
	}

	invalid = valid
	invalid.Cache.PositiveTtlMin = 0
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for zero positive ttl min")
	}

	invalid = valid
	invalid.Cache.PositiveTtlMax = 0
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for zero positive ttl max")
	}

	invalid = valid
	invalid.Cache.NegativeTtl = 0
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for zero negative ttl")
	}

	invalid = valid
	invalid.Cache.PrefetchThreshold = 0
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for zero prefetch threshold")
	}

	invalid = valid
	invalid.Upstreams[0].Transport = "invalid"
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for invalid upstream transport")
	}

	invalid = valid
	invalid.DNSSEC.Mode = "strict"
	if err := validateEngineConfig(invalid); err == nil {
		t.Fatal("expected error for unsupported DNSSEC mode")
	}
}

func TestIsValidUpstreamAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
		want    bool
	}{
		{name: "valid IPv4", address: "1.1.1.1", want: true},
		{name: "valid IPv6", address: "2001:4860:4860::8888", want: true},
		{name: "valid hostname", address: "dns.google", want: true},
		{name: "invalid dotted numeric", address: "999.999.999.999", want: false},
		{name: "invalid hostname leading dash", address: "-bad.host", want: false},
		{name: "uppercase hostname not DNS1123", address: "DNS.GOOGLE", want: false},
		{name: "empty", address: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidUpstreamAddress(tt.address)
			if got != tt.want {
				t.Fatalf("isValidUpstreamAddress(%q) = %v, want %v", tt.address, got, tt.want)
			}
		})
	}
}

func TestValidateEngineConfigRejectsPaddedUpstreamAddress(t *testing.T) {
	cfg := defaultEngineConfig("127.0.0.1:5354")
	cfg.Upstreams[0].Address = " 1.1.1.1"

	if err := validateEngineConfig(cfg); err == nil {
		t.Fatal("expected error for upstream address with leading whitespace")
	}
}

func TestValidateEngineConfigRejectsDuplicateUpstreams(t *testing.T) {
	cfg := defaultEngineConfig("127.0.0.1:5354")
	cfg.Upstreams = append(cfg.Upstreams, cfg.Upstreams[0])

	if err := validateEngineConfig(cfg); err == nil {
		t.Fatal("expected error for duplicate upstream entries")
	}
}

func TestLoadEngineConfigDefaultsDoTAndDoHPorts(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")

	cfg := `{
		"upstreams": [
			{"address": "dns.quad9.net", "transport": "dot"},
			{"address": "dns.google", "transport": "doh"}
		]
	}`
	if err := os.WriteFile(configFile, []byte(cfg), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	loaded, err := loadEngineConfig(configFile, "127.0.0.1:5354")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if loaded.Upstreams[0].Port != 853 {
		t.Fatalf("expected DoT default port 853, got %d", loaded.Upstreams[0].Port)
	}
	if loaded.Upstreams[1].Port != 443 {
		t.Fatalf("expected DoH default port 443, got %d", loaded.Upstreams[1].Port)
	}
}

func TestReportComponentErrCallsOnDropWhenBufferFull(t *testing.T) {
	errs := make(chan error, 1)
	errs <- os.ErrClosed

	var dropped atomic.Int32
	reportComponentErr(errs, os.ErrInvalid, func() {
		dropped.Add(1)
	})

	if dropped.Load() != 1 {
		t.Fatalf("expected one dropped component error callback, got %d", dropped.Load())
	}
}

type mockReloadEngine struct {
	configureErr error
	reloadErr    error
}

func (m *mockReloadEngine) Configure(_ context.Context, _ engine.EngineConfig) (string, error) {
	if m.configureErr != nil {
		return "", m.configureErr
	}
	return "ok", nil
}

func (m *mockReloadEngine) Start(context.Context) error { return nil }

func (m *mockReloadEngine) Reload(context.Context) error {
	return m.reloadErr
}

func (m *mockReloadEngine) Stop(context.Context) error { return nil }

func (m *mockReloadEngine) Capabilities() engine.EngineCapabilities {
	return engine.EngineCapabilities{}
}

func (m *mockReloadEngine) HealthStatus(context.Context) (engine.EngineHealthStatus, error) {
	return engine.EngineHealthStatus{Healthy: true}, nil
}

func (m *mockReloadEngine) HealthCheck(context.Context) (bool, error) {
	return true, nil
}

func (m *mockReloadEngine) Name() engine.EngineType { return "mock" }

func TestApplyConfigReload(t *testing.T) {
	eng := &mockReloadEngine{}
	if err := applyConfigReload(context.Background(), eng, defaultEngineConfig("127.0.0.1:5354")); err != nil {
		t.Fatalf("expected successful apply, got error: %v", err)
	}

	eng = &mockReloadEngine{reloadErr: os.ErrInvalid}
	if err := applyConfigReload(context.Background(), eng, defaultEngineConfig("127.0.0.1:5354")); err == nil {
		t.Fatal("expected error when reload fails")
	}
}
