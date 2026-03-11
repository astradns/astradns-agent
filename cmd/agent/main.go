package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	_ "github.com/astradns/astradns-agent/pkg/engine/coredns"
	_ "github.com/astradns/astradns-agent/pkg/engine/powerdns"
	_ "github.com/astradns/astradns-agent/pkg/engine/unbound"
	"github.com/astradns/astradns-agent/pkg/health"
	"github.com/astradns/astradns-agent/pkg/logging"
	"github.com/astradns/astradns-agent/pkg/metrics"
	"github.com/astradns/astradns-agent/pkg/proxy"
	"github.com/astradns/astradns-agent/pkg/watcher"
	"github.com/astradns/astradns-types/engine"
)

const (
	defaultEngineType   = "unbound"
	defaultListenAddr   = "0.0.0.0:5353"
	defaultEngineAddr   = "127.0.0.1:5354"
	defaultMetricsAddr  = ":9153"
	defaultHealthAddr   = ":8080"
	defaultConfigPath   = "/etc/astradns/config/config.json"
	defaultEngineCfgDir = "/var/run/astradns/engine"
	defaultConfigFile   = "config.json"
	defaultLogMode      = "sampled"
	defaultLogSample    = 0.1
	defaultWorkerBuffer = 10000
)

type runtimeConfig struct {
	EngineType      string
	ListenAddr      string
	EngineAddr      string
	MetricsAddr     string
	HealthAddr      string
	ConfigSourceDir string
	ConfigFile      string
	EngineConfigDir string
	LogMode         logging.LogMode
	LogSampleRate   float64
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := loadRuntimeConfig()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	eng, err := engine.New(engine.EngineType(cfg.EngineType), cfg.EngineConfigDir)
	if err != nil {
		logger.Error("failed to create engine", "error", err, "engine_type", cfg.EngineType)
		os.Exit(1)
	}
	logger.Info("engine selected", "engine_type", eng.Name())

	engineConfig, err := loadEngineConfig(cfg.ConfigFile, cfg.EngineAddr)
	if err != nil {
		logger.Error("failed to load engine config", "error", err, "path", cfg.ConfigFile)
		os.Exit(1)
	}

	generatedPath, err := eng.Configure(ctx, engineConfig)
	if err != nil {
		logger.Error("failed to configure engine", "error", err)
		os.Exit(1)
	}
	logger.Info("engine configured", "config_file", generatedPath)

	if err := eng.Start(ctx); err != nil {
		logger.Error("failed to start engine", "error", err)
		os.Exit(1)
	}

	proxyInstance := proxy.New(proxy.ProxyConfig{
		ListenAddr:    cfg.ListenAddr,
		EngineAddr:    cfg.EngineAddr,
		EventChanSize: defaultWorkerBuffer,
	})

	collector := metrics.NewCollector(nil)
	queryLogger := logging.NewQueryLogger(logging.LoggerConfig{
		Mode:       cfg.LogMode,
		SampleRate: cfg.LogSampleRate,
	})

	upstreams := make([]health.UpstreamTarget, 0, len(engineConfig.Upstreams))
	for _, upstream := range engineConfig.Upstreams {
		upstreams = append(upstreams, health.UpstreamTarget{Address: upstream.Address, Port: upstream.Port})
	}

	checker := health.NewChecker(health.CheckerConfig{
		Upstreams:        upstreams,
		IntervalSeconds:  10,
		TimeoutSeconds:   2,
		FailureThreshold: 3,
	}, collector)
	checker.CheckNow(ctx)

	metricsEvents := make(chan proxy.QueryEvent, defaultWorkerBuffer)
	loggerEvents := make(chan proxy.QueryEvent, defaultWorkerBuffer)

	metricsServer := &http.Server{
		Addr: cfg.MetricsAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				http.NotFound(w, r)
				return
			}
			collector.Handler().ServeHTTP(w, r)
		}),
	}

	healthServer := &http.Server{
		Addr:    cfg.HealthAddr,
		Handler: newHealthHandler(eng, checker),
	}

	componentErrs := make(chan error, 5)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := proxyInstance.Start(ctx); err != nil {
			componentErrs <- fmt.Errorf("proxy failed: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		fanOut(proxyInstance.Events(), func() {
			collector.FanOutDroppedEventsTotal.Inc()
		}, metricsEvents, loggerEvents)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		collector.Run(ctx, metricsEvents)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		queryLogger.Run(ctx, loggerEvents)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		checker.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		trackDroppedEvents(ctx, proxyInstance, collector)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			componentErrs <- fmt.Errorf("metrics server failed: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			componentErrs <- fmt.Errorf("health server failed: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		configWatcher := watcher.New(cfg.ConfigSourceDir, func(ctx context.Context) error {
			newConfig, err := loadEngineConfig(cfg.ConfigFile, cfg.EngineAddr)
			if err != nil {
				collector.AgentConfigReloadErrorsTotal.Inc()
				return fmt.Errorf("reload config: %w", err)
			}
			if _, err := eng.Configure(ctx, newConfig); err != nil {
				collector.AgentConfigReloadErrorsTotal.Inc()
				return fmt.Errorf("reconfigure engine: %w", err)
			}
			if err := eng.Reload(ctx); err != nil {
				collector.AgentConfigReloadErrorsTotal.Inc()
				return fmt.Errorf("reload engine: %w", err)
			}
			collector.AgentConfigReloadTotal.Inc()
			logger.Info("engine config reloaded successfully")
			return nil
		}, logger)
		if err := configWatcher.Run(ctx); err != nil {
			componentErrs <- fmt.Errorf("config watcher failed: %w", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-componentErrs:
		runErr = err
		logger.Error("component stopped with error", "error", err)
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	proxyInstance.Stop()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown failed", "error", err)
	}
	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("health server shutdown failed", "error", err)
	}
	if err := eng.Stop(shutdownCtx); err != nil {
		logger.Error("engine shutdown failed", "error", err)
	}

	wg.Wait()
	collector.AgentUp.Set(0)

	if runErr != nil {
		os.Exit(1)
	}
}

func loadRuntimeConfig() runtimeConfig {
	configSourceDir, configFile := resolveConfigPaths(getEnv("ASTRADNS_CONFIG_PATH", defaultConfigPath))

	return runtimeConfig{
		EngineType:      getEnv("ASTRADNS_ENGINE_TYPE", defaultEngineType),
		ListenAddr:      getEnv("ASTRADNS_LISTEN_ADDR", defaultListenAddr),
		EngineAddr:      getEnv("ASTRADNS_ENGINE_ADDR", defaultEngineAddr),
		MetricsAddr:     getEnv("ASTRADNS_METRICS_ADDR", defaultMetricsAddr),
		HealthAddr:      getEnv("ASTRADNS_HEALTH_ADDR", defaultHealthAddr),
		ConfigSourceDir: configSourceDir,
		ConfigFile:      configFile,
		EngineConfigDir: getEnv("ASTRADNS_ENGINE_CONFIG_DIR", defaultEngineCfgDir),
		LogMode:         logging.LogMode(getEnv("ASTRADNS_LOG_MODE", defaultLogMode)),
		LogSampleRate:   getEnvFloat("ASTRADNS_LOG_SAMPLE_RATE", defaultLogSample),
	}
}

func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func getEnvFloat(key string, fallback float64) float64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func resolveConfigPaths(configPath string) (string, string) {
	info, err := os.Stat(configPath)
	if err == nil {
		if info.IsDir() {
			return configPath, filepath.Join(configPath, defaultConfigFile)
		}
		return filepath.Dir(configPath), configPath
	}

	if filepath.Ext(configPath) == ".json" {
		return filepath.Dir(configPath), configPath
	}

	return configPath, filepath.Join(configPath, defaultConfigFile)
}

func loadEngineConfig(configFile, engineAddr string) (engine.EngineConfig, error) {
	bytes, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultEngineConfig(engineAddr), nil
		}
		return engine.EngineConfig{}, err
	}

	var cfg engine.EngineConfig
	if err := json.Unmarshal(bytes, &cfg); err != nil {
		return engine.EngineConfig{}, err
	}

	if cfg.ListenAddr == "" || cfg.ListenPort == 0 {
		host, port := splitHostPort(engineAddr, "127.0.0.1", 5354)
		if cfg.ListenAddr == "" {
			cfg.ListenAddr = host
		}
		if cfg.ListenPort == 0 {
			cfg.ListenPort = port
		}
	}

	if len(cfg.Upstreams) == 0 {
		cfg.Upstreams = defaultEngineConfig(engineAddr).Upstreams
	}

	if cfg.Cache.MaxEntries == 0 {
		cfg.Cache = defaultEngineConfig(engineAddr).Cache
	}

	return cfg, nil
}

func defaultEngineConfig(engineAddr string) engine.EngineConfig {
	host, port := splitHostPort(engineAddr, "127.0.0.1", 5354)
	return engine.EngineConfig{
		Upstreams: []engine.UpstreamConfig{
			{Address: "1.1.1.1", Port: 53},
		},
		Cache: engine.CacheConfig{
			MaxEntries:        10000,
			PositiveTtlMin:    30,
			PositiveTtlMax:    300,
			NegativeTtl:       30,
			PrefetchEnabled:   true,
			PrefetchThreshold: 10,
		},
		ListenAddr: host,
		ListenPort: port,
	}
}

func splitHostPort(addr, defaultHost string, defaultPort int) (string, int) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return defaultHost, defaultPort
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return defaultHost, defaultPort
	}
	if host == "" {
		host = defaultHost
	}
	return host, port
}

func newHealthHandler(eng engine.Engine, checker *health.Checker) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		healthy, err := eng.HealthCheck(ctx)
		if err != nil || !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unhealthy"))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		healthy, err := eng.HealthCheck(ctx)
		if err != nil || !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("engine unhealthy"))
			return
		}

		if !checker.HasHealthyUpstream() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("no healthy upstreams"))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	return mux
}

func fanOut(in <-chan proxy.QueryEvent, onDrop func(), outs ...chan<- proxy.QueryEvent) {
	for event := range in {
		for _, out := range outs {
			select {
			case out <- event:
			default:
				if onDrop != nil {
					onDrop()
				}
			}
		}
	}

	for _, out := range outs {
		close(out)
	}
}

func trackDroppedEvents(ctx context.Context, p *proxy.Proxy, collector *metrics.Collector) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var last int64
	for {
		select {
		case <-ctx.Done():
			current := p.DroppedEvents()
			if current > last {
				collector.ProxyDroppedEventsTotal.Add(float64(current - last))
			}
			return
		case <-ticker.C:
			current := p.DroppedEvents()
			if current > last {
				collector.ProxyDroppedEventsTotal.Add(float64(current - last))
				last = current
			}
		}
	}
}
