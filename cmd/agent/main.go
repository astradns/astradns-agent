package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
	"runtime"
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
	agenttracing "github.com/astradns/astradns-agent/pkg/tracing"
	"github.com/astradns/astradns-agent/pkg/watcher"
	"github.com/astradns/astradns-types/engine"
	"github.com/miekg/dns"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const (
	defaultEngineType    = "unbound"
	defaultListenAddr    = "0.0.0.0:5353"
	defaultEngineAddr    = "127.0.0.1:5354"
	defaultMetricsAddr   = ":9153"
	defaultHealthAddr    = ":8080"
	defaultConfigPath    = "/etc/astradns/config/config.json"
	defaultEngineCfgDir  = "/var/run/astradns/engine"
	defaultConfigFile    = "config.json"
	defaultLogMode       = "sampled"
	defaultLogSample     = 0.1
	defaultProxyTimeout  = 2 * time.Second
	defaultProxyCacheTTL = 30 * time.Second

	defaultProxyGlobalRateLimitRPS          = 2000
	defaultProxyGlobalRateLimitBurst        = 4000
	defaultProxyPerSourceRateLimitRPS       = 200
	defaultProxyPerSourceRateLimitBurst     = 400
	defaultProxyPerSourceRateLimitStateTTL  = 5 * time.Minute
	defaultProxyPerSourceRateLimitMaxSource = 10000
	defaultProxyCacheMaxEntries             = 10000
	defaultEngineConnPoolSize               = 64
	defaultHealthProbeDomain                = "."
	defaultHealthProbeType                  = dns.TypeNS
	defaultMetricsBearerToken               = ""
	defaultTracingEnabled                   = false
	defaultTracingServiceName               = "astradns-agent"
	defaultTracingEndpoint                  = "localhost:4318"
	defaultTracingInsecure                  = true
	defaultTracingSampleRatio               = 0.1

	defaultEngineRecoveryInterval = 5 * time.Second
	defaultWorkerBuffer           = 10000
	defaultComponentErrorBuffer   = 5
	defaultConfigWatchDebounce    = time.Second
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
	ProxyCacheMaxEntries            int
	ProxyCacheDefaultTTL            time.Duration
	EngineConnectionPoolSize        int
	HealthProbeDomain               string
	HealthProbeType                 uint16
	ConfigWatchDebounce             time.Duration
	MetricsBearerToken              string

	EngineRecoveryInterval time.Duration
	ComponentErrorBuffer   int
	TracingEnabled         bool
	TracingEndpoint        string
	TracingInsecure        bool
	TracingSampleRatio     float64
	TracingServiceName     string
	LogMode                logging.LogMode
	LogSampleRate          float64
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	cfg := loadRuntimeConfig()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	traceShutdown := func(context.Context) error { return nil }
	if cfg.TracingEnabled {
		shutdown, err := agenttracing.Setup(ctx, agenttracing.Config{
			ServiceName:  cfg.TracingServiceName,
			Endpoint:     cfg.TracingEndpoint,
			Insecure:     cfg.TracingInsecure,
			SampleRatio:  cfg.TracingSampleRatio,
			InstanceName: cfg.ListenAddr,
		})
		if err != nil {
			logger.Warn("failed to initialize OpenTelemetry tracing", "error", err)
		} else {
			traceShutdown = shutdown
			logger.Info(
				"OpenTelemetry tracing enabled",
				"endpoint", cfg.TracingEndpoint,
				"sample_ratio", cfg.TracingSampleRatio,
				"service_name", cfg.TracingServiceName,
			)
		}
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		if err := traceShutdown(shutdownCtx); err != nil {
			logger.Warn("failed to shutdown OpenTelemetry tracing", "error", err)
		}
	}()

	tracer := otel.Tracer("astradns-agent")

	baseEngine, err := engine.New(engine.EngineType(cfg.EngineType), cfg.EngineConfigDir)
	if err != nil {
		logger.Error("failed to create engine", "error", err, "engine_type", cfg.EngineType)
		os.Exit(1)
	}
	eng := &synchronizedEngine{inner: baseEngine}
	logger.Info("engine selected", "engine_type", eng.Name())
	capabilities := eng.Capabilities()
	logger.Info(
		"engine capabilities",
		"supports_hot_reload", capabilities.SupportsHotReload,
		"supported_transports", capabilities.SupportedTransports,
		"supported_dnssec_modes", capabilities.SupportedDNSSECModes,
		"supports_tls_server_name", capabilities.SupportsTLSServerName,
		"supports_weighted_upstreams", capabilities.SupportsWeightedUpstreams,
		"supports_priority_upstreams", capabilities.SupportsPriorityUpstreams,
	)

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
		EngineConnectionPoolSize:     cfg.EngineConnectionPoolSize,
		EventChanSize:                defaultWorkerBuffer,
		CacheMaxEntries:              cfg.ProxyCacheMaxEntries,
		CacheDefaultTTL:              cfg.ProxyCacheDefaultTTL,
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
		Addr:    cfg.MetricsAddr,
		Handler: newMetricsHandler(collector, cfg.MetricsBearerToken),
	}

	healthServer := &http.Server{
		Addr:    cfg.HealthAddr,
		Handler: newHealthHandler(eng, checker),
	}

	componentErrs := make(chan error, cfg.ComponentErrorBuffer)
	onComponentErrDrop := func() {
		collector.ComponentErrorBufferOverflowsTotal.Inc()
	}
	var wg sync.WaitGroup

	startComponent(&wg, componentErrs, onComponentErrDrop, "proxy", func() error {
		return proxyInstance.Start(ctx)
	})

	startComponent(&wg, componentErrs, onComponentErrDrop, "fanout", func() error {
		fanOut(proxyInstance.Events(), func() {
			collector.FanOutDroppedEventsTotal.Inc()
		}, metricsEvents, loggerEvents)
		return nil
	})

	startComponent(&wg, componentErrs, onComponentErrDrop, "metrics collector", func() error {
		collector.Run(ctx, metricsEvents)
		return nil
	})

	startComponent(&wg, componentErrs, onComponentErrDrop, "query logger", func() error {
		queryLogger.Run(ctx, loggerEvents)
		return nil
	})

	startComponent(&wg, componentErrs, onComponentErrDrop, "health checker", func() error {
		checker.Run(ctx)
		return nil
	})

	startComponent(&wg, componentErrs, onComponentErrDrop, "metrics server", func() error {
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	startComponent(&wg, componentErrs, onComponentErrDrop, "health server", func() error {
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	startComponent(&wg, componentErrs, onComponentErrDrop, "config watcher", func() error {
		configWatcher := watcher.New(cfg.ConfigSourceDir, func(ctx context.Context) error {
			reloadCtx, span := tracer.Start(ctx, "agent.config_reload")
			defer span.End()
			span.SetAttributes(attribute.String("source", "config-watcher"))

			beforeHash := hashEngineConfig(currentConfig)
			span.SetAttributes(attribute.String("before_hash", beforeHash))
			newConfig, err := loadEngineConfig(cfg.ConfigFile, cfg.EngineAddr)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "read_failed")
				logger.Warn(
					"config reload audit",
					"source", "config-watcher",
					"status", "read_failed",
					"before_hash", beforeHash,
					"error", err,
				)
				collector.AgentConfigReloadErrorsTotal.Inc()
				return fmt.Errorf("reload config: %w", err)
			}
			afterHash := hashEngineConfig(newConfig)
			span.SetAttributes(attribute.String("after_hash", afterHash))
			logger.Info(
				"config reload audit",
				"source", "config-watcher",
				"status", "attempt",
				"before_hash", beforeHash,
				"after_hash", afterHash,
			)
			if err := validateEngineConfig(newConfig); err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "validation_failed")
				logger.Warn(
					"config reload audit",
					"source", "config-watcher",
					"status", "validation_failed",
					"before_hash", beforeHash,
					"after_hash", afterHash,
					"error", err,
				)
				collector.AgentConfigReloadErrorsTotal.Inc()
				return fmt.Errorf("validate config: %w", err)
			}
			if beforeHash == afterHash {
				logger.Info(
					"config reload audit",
					"source", "config-watcher",
					"status", "no_change",
					"before_hash", beforeHash,
					"after_hash", afterHash,
				)
			}
			if err := applyConfigReload(reloadCtx, eng, newConfig); err != nil {
				span.RecordError(err)
				collector.AgentConfigReloadErrorsTotal.Inc()
				if reloadCtx.Err() != nil {
					span.SetStatus(codes.Error, "canceled")
					logger.Warn(
						"config reload audit",
						"source", "config-watcher",
						"status", "canceled",
						"before_hash", beforeHash,
						"after_hash", afterHash,
						"error", err,
					)
					return err
				}
				rollbackErr := applyConfigReload(reloadCtx, eng, currentConfig)
				if rollbackErr != nil {
					span.RecordError(rollbackErr)
					span.SetStatus(codes.Error, "rollback_failed")
					logger.Error(
						"config reload audit",
						"source", "config-watcher",
						"status", "rollback_failed",
						"before_hash", beforeHash,
						"after_hash", afterHash,
						"error", err,
						"rollback_error", rollbackErr,
					)
					return fmt.Errorf("reload engine: %w (rollback failed: %v)", err, rollbackErr)
				}
				span.SetStatus(codes.Error, "rolled_back")
				logger.Warn(
					"config reload audit",
					"source", "config-watcher",
					"status", "rolled_back",
					"before_hash", beforeHash,
					"after_hash", afterHash,
					"error", err,
				)
				return fmt.Errorf("reload engine: %w (rolled back)", err)
			}

			checker.UpdateUpstreams(upstreamTargetsFromConfig(newConfig))
			checker.CheckNow(reloadCtx)
			currentConfig = newConfig
			collector.AgentConfigReloadTotal.Inc()
			span.SetStatus(codes.Ok, "applied")
			logger.Info(
				"config reload audit",
				"source", "config-watcher",
				"status", "applied",
				"before_hash", beforeHash,
				"after_hash", afterHash,
			)
			logger.Info("engine config reloaded successfully")
			return nil
		}, logger, watcher.WithDebounce(cfg.ConfigWatchDebounce))
		return configWatcher.Run(ctx)
	})

	startComponent(&wg, componentErrs, onComponentErrDrop, "engine supervisor", func() error {
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

func (e *synchronizedEngine) Capabilities() engine.EngineCapabilities {
	return e.inner.Capabilities()
}

func (e *synchronizedEngine) HealthStatus(ctx context.Context) (engine.EngineHealthStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inner.HealthStatus(ctx)
}

func (e *synchronizedEngine) HealthCheck(ctx context.Context) (bool, error) {
	status, err := e.HealthStatus(ctx)
	return status.Healthy, err
}

func (e *synchronizedEngine) Name() engine.EngineType {
	return e.inner.Name()
}

type componentFn func() error

func startComponent(
	wg *sync.WaitGroup,
	errs chan<- error,
	onDrop func(),
	name string,
	fn componentFn,
) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				reportComponentErr(errs, fmt.Errorf("%s panicked: %v\n%s", name, recovered, string(debug.Stack())), onDrop)
			}
		}()

		if err := fn(); err != nil && !errors.Is(err, context.Canceled) {
			reportComponentErr(errs, fmt.Errorf("%s failed: %w", name, err), onDrop)
		}
	}()
}

func reportComponentErr(errs chan<- error, err error, onDrop func()) {
	select {
	case errs <- err:
	default:
		if onDrop != nil {
			onDrop()
		}
	}
}

func validateEngineConfig(cfg engine.EngineConfig) error {
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return fmt.Errorf("listenAddr must not be empty")
	}
	if cfg.ListenPort <= 0 || cfg.ListenPort > 65535 {
		return fmt.Errorf("listenPort must be between 1 and 65535")
	}
	if cfg.WorkerThreads <= 0 || cfg.WorkerThreads > 256 {
		return fmt.Errorf("workerThreads must be between 1 and 256")
	}
	if !isSupportedDNSSECMode(cfg.DNSSEC.Mode) {
		return fmt.Errorf("dnssec.mode must be one of off, process, or validate")
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

		transport, ok := parseUpstreamTransport(upstream.Transport)
		if !ok {
			return fmt.Errorf("upstreams[%d].transport must be one of dns, dot, or doh", i)
		}

		port := upstream.Port
		if port == 0 {
			port = defaultPortForTransport(transport)
		}
		if port <= 0 || port > 65535 {
			return fmt.Errorf("upstreams[%d].port must be between 1 and 65535", i)
		}

		tlsServerName := strings.TrimSpace(upstream.TLSServerName)
		if transport == engine.UpstreamTransportDNS && tlsServerName != "" {
			return fmt.Errorf("upstreams[%d].tlsServerName is only valid for dot or doh transport", i)
		}
		if tlsServerName != "" && !isDNS1123Subdomain(strings.TrimSuffix(strings.ToLower(tlsServerName), ".")) {
			return fmt.Errorf("upstreams[%d].tlsServerName must be a valid DNS hostname", i)
		}
		if upstream.Weight <= 0 || upstream.Weight > 100 {
			return fmt.Errorf("upstreams[%d].weight must be between 1 and 100", i)
		}
		if upstream.Preference <= 0 || upstream.Preference > 1000 {
			return fmt.Errorf("upstreams[%d].preference must be between 1 and 1000", i)
		}

		upstreamKey := fmt.Sprintf("%s:%s:%d", transport, trimmedAddress, port)
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

func parseUpstreamTransport(raw engine.UpstreamTransport) (engine.UpstreamTransport, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(string(raw)))
	switch engine.UpstreamTransport(trimmed) {
	case "", engine.UpstreamTransportDNS:
		return engine.UpstreamTransportDNS, true
	case engine.UpstreamTransportDoT:
		return engine.UpstreamTransportDoT, true
	case engine.UpstreamTransportDoH:
		return engine.UpstreamTransportDoH, true
	default:
		return "", false
	}
}

func defaultPortForTransport(transport engine.UpstreamTransport) int32 {
	switch transport {
	case engine.UpstreamTransportDoT:
		return 853
	case engine.UpstreamTransportDoH:
		return 443
	default:
		return 53
	}
}

func isSupportedDNSSECMode(mode engine.DNSSECMode) bool {
	trimmed := strings.ToLower(strings.TrimSpace(string(mode)))
	switch engine.DNSSECMode(trimmed) {
	case "", engine.DNSSECModeOff, engine.DNSSECModeProcess, engine.DNSSECModeValidate:
		return true
	default:
		return false
	}
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

func hashEngineConfig(cfg engine.EngineConfig) string {
	bytes, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}

	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
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
		ProxyCacheMaxEntries:            getEnvNonNegativeInt("ASTRADNS_PROXY_CACHE_MAX_ENTRIES", defaultProxyCacheMaxEntries),
		ProxyCacheDefaultTTL:            getEnvDuration("ASTRADNS_PROXY_CACHE_DEFAULT_TTL", defaultProxyCacheTTL),
		EngineConnectionPoolSize:        getEnvPositiveInt("ASTRADNS_ENGINE_CONN_POOL_SIZE", defaultEngineConnPoolSize),
		HealthProbeDomain:               getEnv("ASTRADNS_HEALTH_PROBE_DOMAIN", defaultHealthProbeDomain),
		HealthProbeType:                 getEnvDNSQueryType("ASTRADNS_HEALTH_PROBE_TYPE", defaultHealthProbeType),
		ConfigWatchDebounce:             getEnvDuration("ASTRADNS_CONFIG_WATCH_DEBOUNCE", defaultConfigWatchDebounce),
		MetricsBearerToken:              strings.TrimSpace(getEnv("ASTRADNS_METRICS_BEARER_TOKEN", defaultMetricsBearerToken)),

		EngineRecoveryInterval: getEnvDuration("ASTRADNS_ENGINE_RECOVERY_INTERVAL", defaultEngineRecoveryInterval),
		ComponentErrorBuffer:   getEnvPositiveInt("ASTRADNS_COMPONENT_ERROR_BUFFER", defaultComponentErrorBuffer),
		TracingEnabled:         getEnvBool("ASTRADNS_TRACING_ENABLED", defaultTracingEnabled),
		TracingEndpoint:        strings.TrimSpace(getEnv("ASTRADNS_TRACING_ENDPOINT", defaultTracingEndpoint)),
		TracingInsecure:        getEnvBool("ASTRADNS_TRACING_INSECURE", defaultTracingInsecure),
		TracingSampleRatio:     getEnvFloat("ASTRADNS_TRACING_SAMPLE_RATIO", defaultTracingSampleRatio),
		TracingServiceName:     strings.TrimSpace(getEnv("ASTRADNS_TRACING_SERVICE_NAME", defaultTracingServiceName)),
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

func getEnvNonNegativeInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}

	return parsed
}

func getEnvBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
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
	if cfg.WorkerThreads == 0 {
		cfg.WorkerThreads = defaults.WorkerThreads
	}
	if cfg.WorkerThreads > 256 {
		cfg.WorkerThreads = 256
	}

	if !isSupportedDNSSECMode(cfg.DNSSEC.Mode) || strings.TrimSpace(string(cfg.DNSSEC.Mode)) == "" {
		cfg.DNSSEC = defaults.DNSSEC
	}

	if len(cfg.Upstreams) == 0 {
		cfg.Upstreams = defaults.Upstreams
	}
	for i := range cfg.Upstreams {
		transport, ok := parseUpstreamTransport(cfg.Upstreams[i].Transport)
		if !ok {
			transport = engine.UpstreamTransportDNS
		}
		cfg.Upstreams[i].Transport = transport
		if cfg.Upstreams[i].Port == 0 {
			cfg.Upstreams[i].Port = defaultPortForTransport(cfg.Upstreams[i].Transport)
		}
		if cfg.Upstreams[i].Transport == engine.UpstreamTransportDNS {
			cfg.Upstreams[i].TLSServerName = ""
		} else {
			cfg.Upstreams[i].TLSServerName = strings.TrimSpace(cfg.Upstreams[i].TLSServerName)
			if cfg.Upstreams[i].TLSServerName == "" {
				cfg.Upstreams[i].TLSServerName = defaultTLSServerName(cfg.Upstreams[i].Address)
			}
		}
		if cfg.Upstreams[i].Weight == 0 {
			cfg.Upstreams[i].Weight = 1
		}
		if cfg.Upstreams[i].Preference == 0 {
			cfg.Upstreams[i].Preference = 100
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
			{Address: "1.1.1.1", Port: 53, Transport: engine.UpstreamTransportDNS, Weight: 1, Preference: 100},
		},
		Cache: engine.CacheConfig{
			MaxEntries:        10000,
			PositiveTtlMin:    30,
			PositiveTtlMax:    300,
			NegativeTtl:       30,
			PrefetchEnabled:   true,
			PrefetchThreshold: 10,
		},
		ListenAddr:    host,
		ListenPort:    int32(port),
		WorkerThreads: resolveDefaultWorkerThreads(),
		DNSSEC:        engine.DNSSECConfig{Mode: engine.DNSSECModeOff},
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
		targets = append(targets, health.UpstreamTarget{
			Address:       upstream.Address,
			Port:          int(upstream.Port),
			Transport:     upstream.Transport,
			TLSServerName: upstream.TLSServerName,
		})
	}

	return targets
}

func resolveDefaultWorkerThreads() int32 {
	auto := int32(runtime.NumCPU())
	if auto <= 0 {
		return 2
	}
	if auto > 256 {
		return 256
	}

	return auto
}

func defaultTLSServerName(address string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(address), ".")
	if trimmed == "" {
		return ""
	}
	if _, err := netip.ParseAddr(trimmed); err == nil {
		return ""
	}
	if !isDNS1123Subdomain(strings.ToLower(trimmed)) {
		return ""
	}

	return trimmed
}

func newMetricsHandler(collector *metrics.Collector, bearerToken string) http.Handler {
	token := strings.TrimSpace(bearerToken)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}

		if token != "" && !isAuthorizedMetricsRequest(r, token) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("unauthorized"))
			return
		}

		collector.Handler().ServeHTTP(w, r)
	})
}

func isAuthorizedMetricsRequest(r *http.Request, token string) bool {
	if r == nil {
		return false
	}

	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(authorization, bearerPrefix) {
		return false
	}

	presentedToken := strings.TrimSpace(strings.TrimPrefix(authorization, bearerPrefix))
	if presentedToken == "" || token == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(presentedToken), []byte(token)) == 1
}

func newHealthHandler(eng engine.Engine, checker *health.Checker) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status, err := eng.HealthStatus(ctx)
		if err != nil || !status.Healthy {
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

		status, err := eng.HealthStatus(ctx)
		if err != nil || !status.Healthy {
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
