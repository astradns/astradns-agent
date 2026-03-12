package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astradns/astradns-agent/pkg/proxy"
	"github.com/astradns/astradns-types/engine"
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

	// Only upstreams specified, no cache or listen fields.
	partial := `{"upstreams": [{"address": "9.9.9.9", "port": 53}]}`
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
