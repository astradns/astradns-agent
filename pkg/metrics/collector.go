package metrics

import (
	"context"
	"net/http"
	"strings"

	"github.com/astradns/astradns-agent/pkg/proxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Collector processes QueryEvents and updates Prometheus metrics.
type Collector struct {
	registry *prometheus.Registry

	QueriesTotal                       prometheus.Counter
	QueriesByType                      *prometheus.CounterVec
	CacheHitsTotal                     prometheus.Counter
	CacheMissesTotal                   prometheus.Counter
	UpstreamQueriesTotal               *prometheus.CounterVec
	UpstreamLatencySeconds             *prometheus.HistogramVec
	UpstreamFailuresTotal              *prometheus.CounterVec
	UpstreamHealthy                    *prometheus.GaugeVec
	NXDomainTotal                      prometheus.Counter
	ServfailTotal                      prometheus.Counter
	TimeoutTotal                       *prometheus.CounterVec
	AgentUp                            prometheus.Gauge
	AgentConfigReloadTotal             prometheus.Counter
	AgentConfigReloadErrorsTotal       prometheus.Counter
	AgentEngineRecoveryAttemptsTotal   prometheus.Counter
	AgentEngineRecoverySuccessTotal    prometheus.Counter
	AgentEngineRecoveryErrorsTotal     prometheus.Counter
	ProxyDroppedEventsTotal            prometheus.Counter
	FanOutDroppedEventsTotal           prometheus.Counter
	ComponentErrorBufferOverflowsTotal prometheus.Counter
	DeniedQueriesTotal                 prometheus.Counter
}

// NewCollector creates a Collector and registers all AstraDNS metrics.
func NewCollector(registry *prometheus.Registry) *Collector {
	if registry == nil {
		registry = prometheus.NewRegistry()
	}

	c := &Collector{
		registry: registry,
		QueriesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_queries_total",
			Help: "Total number of DNS queries observed by the proxy.",
		}),
		QueriesByType: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "astradns_queries_by_type",
			Help: "Total number of DNS queries by query type.",
		}, []string{"qtype"}),
		CacheHitsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_cache_hits_total",
			Help: "Total number of cache hits.",
		}),
		CacheMissesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_cache_misses_total",
			Help: "Total number of cache misses.",
		}),
		UpstreamQueriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "astradns_upstream_queries_total",
			Help: "Total number of upstream queries by upstream target.",
		}, []string{"upstream"}),
		UpstreamLatencySeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "astradns_upstream_latency_seconds",
			Help:    "Latency of upstream DNS responses in seconds.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}, []string{"upstream"}),
		UpstreamFailuresTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "astradns_upstream_failures_total",
			Help: "Total number of upstream failures.",
		}, []string{"upstream"}),
		UpstreamHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "astradns_upstream_healthy",
			Help: "Health status of upstream resolvers (1=healthy, 0=unhealthy).",
		}, []string{"upstream"}),
		NXDomainTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_nxdomain_total",
			Help: "Total number of NXDOMAIN responses.",
		}),
		ServfailTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_servfail_total",
			Help: "Total number of SERVFAIL responses.",
		}),
		TimeoutTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "astradns_timeout_total",
			Help: "Total number of upstream timeout responses.",
		}, []string{"upstream"}),
		AgentUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "astradns_agent_up",
			Help: "Whether the AstraDNS agent process is up (1) or down (0).",
		}),
		AgentConfigReloadTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_agent_config_reload_total",
			Help: "Total number of attempted config reloads.",
		}),
		AgentConfigReloadErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_agent_config_reload_errors_total",
			Help: "Total number of failed config reloads.",
		}),
		AgentEngineRecoveryAttemptsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_agent_engine_recovery_attempts_total",
			Help: "Total number of engine recovery attempts.",
		}),
		AgentEngineRecoverySuccessTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_agent_engine_recovery_success_total",
			Help: "Total number of successful engine recoveries.",
		}),
		AgentEngineRecoveryErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_agent_engine_recovery_errors_total",
			Help: "Total number of failed engine recoveries.",
		}),
		ProxyDroppedEventsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_proxy_dropped_events_total",
			Help: "Total number of query events dropped by the proxy pipeline.",
		}),
		FanOutDroppedEventsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_fanout_dropped_events_total",
			Help: "Total number of query events dropped while fanning out to workers.",
		}),
		ComponentErrorBufferOverflowsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_component_error_buffer_overflows_total",
			Help: "Total number of dropped component errors when the supervisor error channel is full.",
		}),
		DeniedQueriesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "astradns_denied_queries_total",
			Help: "Total number of DNS queries denied by domain filter rules.",
		}),
	}

	registry.MustRegister(
		c.QueriesTotal,
		c.QueriesByType,
		c.CacheHitsTotal,
		c.CacheMissesTotal,
		c.UpstreamQueriesTotal,
		c.UpstreamLatencySeconds,
		c.UpstreamFailuresTotal,
		c.UpstreamHealthy,
		c.NXDomainTotal,
		c.ServfailTotal,
		c.TimeoutTotal,
		c.AgentUp,
		c.AgentConfigReloadTotal,
		c.AgentConfigReloadErrorsTotal,
		c.AgentEngineRecoveryAttemptsTotal,
		c.AgentEngineRecoverySuccessTotal,
		c.AgentEngineRecoveryErrorsTotal,
		c.ProxyDroppedEventsTotal,
		c.FanOutDroppedEventsTotal,
		c.ComponentErrorBufferOverflowsTotal,
		c.DeniedQueriesTotal,
	)

	c.AgentUp.Set(1)

	return c
}

// ProcessEvent updates metrics from a single QueryEvent.
func (c *Collector) ProcessEvent(event proxy.QueryEvent) {
	qType := normalizeQueryTypeLabel(event.QueryType)

	upstream := event.Upstream
	if upstream == "" {
		upstream = "unknown"
	}

	c.QueriesTotal.Inc()
	c.QueriesByType.WithLabelValues(qType).Inc()

	if event.Denied {
		c.DeniedQueriesTotal.Inc()
	}

	if event.CacheHitKnown {
		if event.CacheHit {
			c.CacheHitsTotal.Inc()
		} else {
			c.CacheMissesTotal.Inc()
		}
	}

	c.UpstreamQueriesTotal.WithLabelValues(upstream).Inc()
	c.UpstreamLatencySeconds.WithLabelValues(upstream).Observe(event.LatencyMs / 1000.0)

	rcode := strings.ToUpper(event.ResponseCode)
	switch rcode {
	case "NXDOMAIN":
		c.NXDomainTotal.Inc()
	case "SERVFAIL":
		c.ServfailTotal.Inc()
		c.UpstreamFailuresTotal.WithLabelValues(upstream).Inc()
	case "TIMEOUT":
		c.TimeoutTotal.WithLabelValues(upstream).Inc()
		c.UpstreamFailuresTotal.WithLabelValues(upstream).Inc()
	}
}

func normalizeQueryTypeLabel(queryType string) string {
	qType := strings.ToUpper(strings.TrimSpace(queryType))
	if qType == "" {
		return "UNKNOWN"
	}

	switch qType {
	case "A", "AAAA", "CAA", "CNAME", "DS", "DNSKEY", "HTTPS", "MX", "NAPTR", "NS", "PTR",
		"RRSIG", "SOA", "SRV", "SVCB", "TXT":
		return qType
	default:
		return "OTHER"
	}
}

// Run consumes events from the channel and processes them.
func (c *Collector) Run(ctx context.Context, events <-chan proxy.QueryEvent) {
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			c.ProcessEvent(event)
		case <-ctx.Done():
			for event := range events {
				c.ProcessEvent(event)
			}
			return
		}
	}
}

// Handler returns the HTTP handler used to expose Prometheus metrics.
func (c *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{})
}
