package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
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
	"github.com/miekg/dns"
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
	defaultProxyTimeout = 2 * time.Second

	defaultProxyGlobalRateLimitRPS          = 2000
	defaultProxyGlobalRateLimitBurst        = 4000
	defaultProxyPerSourceRateLimitRPS       = 200
	defaultProxyPerSourceRateLimitBurst     = 400
	defaultProxyPerSourceRateLimitStateTTL  = 5 * time.Minute
	defaultProxyPerSourceRateLimitMaxSource = 10000
	defaultHealthProbeDomain                = "."
	defaultHealthProbeType                  = dns.TypeNS

	defaultEngineRecoveryInterval = 5 * time.Second
	defaultWorkerBuffer           = 10000
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
	ProxyTimeout    time.Duration

	ProxyGlobalRateLimitRPS         float64
	ProxyGlobalRateLimitBurst       int
	ProxyPerSourceRateLimitRPS      float64
	ProxyPerSourceRateLimitBurst    int
	ProxyPerSourceRateLimitStateTTL time.Duration
	ProxyPerSourceRateLimitMaxSrc   int
	HealthProbeDomain               string
	HealthProbeType                 uint16

	EngineRecoveryInterval time.Duration
	LogMode                logging.LogMode
	LogSampleRate          float64
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	cfg := loadRuntimeConfig()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	baseEngine, err := engine.New(engine.EngineType(cfg.EngineType), cfg.EngineConfigDir)
	if err != nil {
		logger.Error("failed to create engine", "error", err, "engine_type", cfg.EngineType)
		os.Exit(1)
	}
	eng := &synchronizedEngine{inner: baseEngine}
	logger.Info("engine selected", "engine_type", eng.Name())

	engineConfig, err := loadEngineConfig(cfg.ConfigFile, cfg.EngineAddr)
	if err != nil {
		logger.Error("failed to load engine config", "error", err, "path", cfg.ConfigFile)
		os.Exit(1)
	}
	if err := validateEngineConfig(engineConfig); err != nil {
		logger.Error("invalid engine config", "error", err, "path", cfg.ConfigFile)
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

	collector := metrics.NewCollector(nil)
	queryLogger := logging.NewQueryLogger(logging.LoggerConfig{
		Mode:       cfg.LogMode,
		SampleRate: cfg.LogSampleRate,
	})

	proxyInstance := proxy.New(proxy.ProxyConfig{
		ListenAddr:                   cfg.ListenAddr,
		EngineAddr:                   cfg.EngineAddr,
		QueryTimeout:                 cfg.ProxyTimeout,
		EventChanSize:                defaultWorkerBuffer,
		GlobalRateLimitRPS:           cfg.ProxyGlobalRateLimitRPS,
		GlobalRateLimitBurst:         cfg.ProxyGlobalRateLimitBurst,
		PerSourceRateLimitRPS:        cfg.ProxyPerSourceRateLimitRPS,
		PerSourceRateLimitBurst:      cfg.ProxyPerSourceRateLimitBurst,
		PerSourceRateLimitStateTTL:   cfg.ProxyPerSourceRateLimitStateTTL,
		PerSourceRateLimitMaxSources: cfg.ProxyPerSourceRateLimitMaxSrc,
		OnEventDrop: func() {
			collector.ProxyDroppedEventsTotal.Inc()
		},
	})
	currentConfig := engineConfig

	upstreams := upstreamTargetsFromConfig(engineConfig)

	checker := health.NewChecker(health.CheckerConfig{
		Upstreams:        upstreams,
		IntervalSeconds:  10,
		TimeoutSeconds:   2,
		FailureThreshold: 3,
		ProbeDomain:      cfg.HealthProbeDomain,
		ProbeType:        cfg.HealthProbeType,
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

	startComponent(&wg, componentErrs, "proxy", func() error {
		return proxyInstance.Start(ctx)
	})

	startComponent(&wg, componentErrs, "fanout", func() error {
		fanOut(proxyInstance.Events(), func() {
			collector.FanOutDroppedEventsTotal.Inc()
		}, metricsEvents, loggerEvents)
		return nil
	})

	startComponent(&wg, componentErrs, "metrics collector", func() error {
		collector.Run(ctx, metricsEvents)
		return nil
	})

	startComponent(&wg, componentErrs, "query logger", func() error {
		queryLogger.Run(ctx, loggerEvents)
		return nil
	})

	startComponent(&wg, componentErrs, "health checker", func() error {
		checker.Run(ctx)
		return nil
	})

	startComponent(&wg, componentErrs, "metrics server", func() error {
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	startComponent(&wg, componentErrs, "health server", func() error {
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	startComponent(&wg, componentErrs, "config watcher", func() error {
		configWatcher := watcher.New(cfg.ConfigSourceDir, func(ctx context.Context) error {
			newConfig, err := loadEngineConfig(cfg.ConfigFile, cfg.EngineAddr)
			if err != nil {
				collector.AgentConfigReloadErrorsTotal.Inc()
				return fmt.Errorf("reload config: %w", err)
			}
			if err := validateEngineConfig(newConfig); err != nil {
				collector.AgentConfigReloadErrorsTotal.Inc()
				return fmt.Errorf("validate config: %w", err)
			}
			if err := applyConfigReload(ctx, eng, newConfig); err != nil {
				collector.AgentConfigReloadErrorsTotal.Inc()
				if ctx.Err() != nil {
					return err
				}
				rollbackErr := applyConfigReload(ctx, eng, currentConfig)
				if rollbackErr != nil {
					return fmt.Errorf("reload engine: %w (rollback failed: %v)", err, rollbackErr)
				}
				return fmt.Errorf("reload engine: %w (rolled back)", err)
			}

			checker.UpdateUpstreams(upstreamTargetsFromConfig(newConfig))
			checker.CheckNow(ctx)
			currentConfig = newConfig
			collector.AgentConfigReloadTotal.Inc()
			logger.Info("engine config reloaded successfully")
			return nil
		}, logger)
		return configWatcher.Run(ctx)
	})

	startComponent(&wg, componentErrs, "engine supervisor", func() error {
		superviseEngine(ctx, eng, cfg.EngineAddr, cfg.EngineRecoveryInterval, logger, collector)
		return nil
	})

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

type synchronizedEngine struct {
	inner engine.Engine
	mu    sync.Mutex
}

func (e *synchronizedEngine) Configure(ctx context.Context, cfg engine.EngineConfig) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inner.Configure(ctx, cfg)
}

func (e *synchronizedEngine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inner.Start(ctx)
}

func (e *synchronizedEngine) Reload(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inner.Reload(ctx)
}

func (e *synchronizedEngine) Stop(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inner.Stop(ctx)
}

func (e *synchronizedEngine) HealthCheck(ctx context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inner.HealthCheck(ctx)
}

func (e *synchronizedEngine) Name() engine.EngineType {
	return e.inner.Name()
}

type componentFn func() error

func startComponent(wg *sync.WaitGroup, errs chan<- error, name string, fn componentFn) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				reportComponentErr(errs, fmt.Errorf("%s panicked: %v\n%s", name, recovered, string(debug.Stack())))
			}
		}()

		if err := fn(); err != nil && !errors.Is(err, context.Canceled) {
			reportComponentErr(errs, fmt.Errorf("%s failed: %w", name, err))
		}
	}()
}

func reportComponentErr(errs chan<- error, err error) {
	select {
	case errs <- err:
	default:
	}
}

func validateEngineConfig(cfg engine.EngineConfig) error {
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return fmt.Errorf("listenAddr must not be empty")
	}
	if cfg.ListenPort <= 0 || cfg.ListenPort > 65535 {
		return fmt.Errorf("listenPort must be between 1 and 65535")
	}
	if len(cfg.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream is required")
	}
	seenUpstreams := make(map[string]struct{}, len(cfg.Upstreams))
	for i, upstream := range cfg.Upstreams {
		trimmedAddress := strings.TrimSpace(upstream.Address)
		if upstream.Address != trimmedAddress {
			return fmt.Errorf("upstreams[%d].address must not include leading or trailing whitespace", i)
		}
		if !isValidUpstreamAddress(trimmedAddress) {
			return fmt.Errorf("upstreams[%d].address must be a valid IP or DNS name", i)
		}
		if upstream.Port <= 0 || upstream.Port > 65535 {
			return fmt.Errorf("upstreams[%d].port must be between 1 and 65535", i)
		}

		upstreamKey := fmt.Sprintf("%s:%d", trimmedAddress, upstream.Port)
		if _, exists := seenUpstreams[upstreamKey]; exists {
			return fmt.Errorf("upstreams[%d] %q is duplicated", i, upstreamKey)
		}
		seenUpstreams[upstreamKey] = struct{}{}
	}
	if cfg.Cache.MaxEntries <= 0 {
		return fmt.Errorf("cache.maxEntries must be greater than zero")
	}
	if cfg.Cache.PositiveTtlMin <= 0 {
		return fmt.Errorf("cache.positiveTtlMin must be greater than zero")
	}
	if cfg.Cache.PositiveTtlMax <= 0 {
		return fmt.Errorf("cache.positiveTtlMax must be greater than zero")
	}
	if cfg.Cache.NegativeTtl <= 0 {
		return fmt.Errorf("cache.negativeTtl must be greater than zero")
	}
	if cfg.Cache.PrefetchThreshold <= 0 {
		return fmt.Errorf("cache.prefetchThreshold must be greater than zero")
	}
	if cfg.Cache.PositiveTtlMin > 0 && cfg.Cache.PositiveTtlMax > 0 && cfg.Cache.PositiveTtlMin > cfg.Cache.PositiveTtlMax {
		return fmt.Errorf("cache.positiveTtlMin must be <= cache.positiveTtlMax")
	}

	return nil
}

func isValidUpstreamAddress(address string) bool {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return false
	}

	if _, err := netip.ParseAddr(trimmed); err == nil {
		return true
	}

	if looksLikeInvalidIPv4Literal(trimmed) {
		return false
	}

	return isDNS1123Subdomain(trimmed)
}

func looksLikeInvalidIPv4Literal(value string) bool {
	if strings.Count(value, ".") != 3 {
		return false
	}

	for _, r := range value {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}

	return true
}

func isDNS1123Subdomain(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}

	labels := strings.Split(value, ".")
	for _, label := range labels {
		if !isDNS1123Label(label) {
			return false
		}
	}

	return true
}

func isDNS1123Label(label string) bool {
	if len(label) == 0 || len(label) > 63 {
		return false
	}

	for i := 0; i < len(label); i++ {
		ch := label[i]
		isLowerAlpha := ch >= 'a' && ch <= 'z'
		isDigit := ch >= '0' && ch <= '9'
		isHyphen := ch == '-'

		if !isLowerAlpha && !isDigit && !isHyphen {
			return false
		}

		if (i == 0 || i == len(label)-1) && isHyphen {
			return false
		}
	}

	return true
}

func applyConfigReload(ctx context.Context, eng engine.Engine, cfg engine.EngineConfig) error {
	if _, err := eng.Configure(ctx, cfg); err != nil {
		return fmt.Errorf("reconfigure engine: %w", err)
	}
	if err := eng.Reload(ctx); err != nil {
		return fmt.Errorf("reload engine: %w", err)
	}

	return nil
}

func superviseEngine(
	ctx context.Context,
	eng engine.Engine,
	engineAddr string,
	interval time.Duration,
	logger *slog.Logger,
	collector *metrics.Collector,
) {
	if interval <= 0 {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, defaultProxyTimeout)
			responsive, err := isEngineResponsive(checkCtx, engineAddr)
			cancel()
			if responsive {
				continue
			}

			collector.AgentEngineRecoveryAttemptsTotal.Inc()
			logger.Warn(
				"engine is not responding, attempting restart",
				"engine_addr", engineAddr,
				"error", err,
			)

			restartCtx, restartCancel := context.WithTimeout(ctx, 5*time.Second)
			if stopErr := eng.Stop(restartCtx); stopErr != nil {
				logger.Warn("failed to stop engine before restart", "error", stopErr)
			}
			startErr := eng.Start(restartCtx)
			restartCancel()

			if startErr != nil {
				collector.AgentEngineRecoveryErrorsTotal.Inc()
				logger.Error("failed to restart engine", "error", startErr)
				continue
			}

			collector.AgentEngineRecoverySuccessTotal.Inc()
			logger.Info("engine restarted successfully")
		}
	}
}

func isEngineResponsive(ctx context.Context, engineAddr string) (bool, error) {
	msg := new(dns.Msg)
	msg.SetQuestion(".", dns.TypeA)

	client := &dns.Client{}
	response, _, err := client.ExchangeContext(ctx, msg, engineAddr)
	if err != nil {
		return false, err
	}
	if response == nil {
		return false, fmt.Errorf("empty DNS response from engine")
	}

	return true, nil
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
		ProxyTimeout:    getEnvDuration("ASTRADNS_PROXY_TIMEOUT", defaultProxyTimeout),

		ProxyGlobalRateLimitRPS:         getEnvPositiveFloat("ASTRADNS_PROXY_RATE_LIMIT_GLOBAL_RPS", defaultProxyGlobalRateLimitRPS),
		ProxyGlobalRateLimitBurst:       getEnvPositiveInt("ASTRADNS_PROXY_RATE_LIMIT_GLOBAL_BURST", defaultProxyGlobalRateLimitBurst),
		ProxyPerSourceRateLimitRPS:      getEnvPositiveFloat("ASTRADNS_PROXY_RATE_LIMIT_PER_SOURCE_RPS", defaultProxyPerSourceRateLimitRPS),
		ProxyPerSourceRateLimitBurst:    getEnvPositiveInt("ASTRADNS_PROXY_RATE_LIMIT_PER_SOURCE_BURST", defaultProxyPerSourceRateLimitBurst),
		ProxyPerSourceRateLimitStateTTL: getEnvDuration("ASTRADNS_PROXY_RATE_LIMIT_PER_SOURCE_STATE_TTL", defaultProxyPerSourceRateLimitStateTTL),
		ProxyPerSourceRateLimitMaxSrc:   getEnvPositiveInt("ASTRADNS_PROXY_RATE_LIMIT_PER_SOURCE_MAX_SOURCES", defaultProxyPerSourceRateLimitMaxSource),
		HealthProbeDomain:               getEnv("ASTRADNS_HEALTH_PROBE_DOMAIN", defaultHealthProbeDomain),
		HealthProbeType:                 getEnvDNSQueryType("ASTRADNS_HEALTH_PROBE_TYPE", defaultHealthProbeType),

		EngineRecoveryInterval: getEnvDuration("ASTRADNS_ENGINE_RECOVERY_INTERVAL", defaultEngineRecoveryInterval),
		LogMode:                logging.LogMode(getEnv("ASTRADNS_LOG_MODE", defaultLogMode)),
		LogSampleRate:          getEnvFloat("ASTRADNS_LOG_SAMPLE_RATE", defaultLogSample),
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
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fallback
	}
	return parsed
}

func getEnvPositiveFloat(key string, fallback float64) float64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return fallback
	}

	return parsed
}

func getEnvPositiveInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}

	return parsed
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func getEnvDNSQueryType(key string, fallback uint16) uint16 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	if parsedType, found := dns.StringToType[strings.ToUpper(value)]; found && parsedType != 0 {
		return parsedType
	}

	parsedType, err := strconv.ParseUint(value, 10, 16)
	if err != nil || parsedType == 0 {
		return fallback
	}

	return uint16(parsedType)
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
	defaults := defaultEngineConfig(engineAddr)

	bytes, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			return defaults, nil
		}
		return engine.EngineConfig{}, err
	}

	var cfg engine.EngineConfig
	if err := json.Unmarshal(bytes, &cfg); err != nil {
		return engine.EngineConfig{}, err
	}

	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaults.ListenAddr
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = defaults.ListenPort
	}

	if len(cfg.Upstreams) == 0 {
		cfg.Upstreams = defaults.Upstreams
	}
	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Port == 0 {
			cfg.Upstreams[i].Port = 53
		}
	}

	if cfg.Cache.MaxEntries == 0 {
		cfg.Cache = defaults.Cache
	} else {
		if cfg.Cache.PositiveTtlMin == 0 {
			cfg.Cache.PositiveTtlMin = defaults.Cache.PositiveTtlMin
		}
		if cfg.Cache.PositiveTtlMax == 0 {
			cfg.Cache.PositiveTtlMax = defaults.Cache.PositiveTtlMax
		}
		if cfg.Cache.NegativeTtl == 0 {
			cfg.Cache.NegativeTtl = defaults.Cache.NegativeTtl
		}
		if cfg.Cache.PrefetchThreshold == 0 {
			cfg.Cache.PrefetchThreshold = defaults.Cache.PrefetchThreshold
		}
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
		ListenPort: int32(port),
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

func upstreamTargetsFromConfig(cfg engine.EngineConfig) []health.UpstreamTarget {
	targets := make([]health.UpstreamTarget, 0, len(cfg.Upstreams))
	for _, upstream := range cfg.Upstreams {
		targets = append(targets, health.UpstreamTarget{Address: upstream.Address, Port: int(upstream.Port)})
	}

	return targets
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
