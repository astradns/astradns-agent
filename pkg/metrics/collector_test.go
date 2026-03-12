package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astradns/astradns-agent/pkg/proxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestProcessEventCacheHitIncrementsCacheHits(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	collector.ProcessEvent(proxy.QueryEvent{QueryType: "A", ResponseCode: "NOERROR", Upstream: "1.1.1.1:53", CacheHitKnown: true, CacheHit: true})

	if got := testutil.ToFloat64(collector.CacheHitsTotal); got != 1 {
		t.Fatalf("expected cache hits = 1, got %v", got)
	}
}

func TestProcessEventCacheMissIncrementsCacheMisses(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	collector.ProcessEvent(proxy.QueryEvent{QueryType: "A", ResponseCode: "NOERROR", Upstream: "1.1.1.1:53", CacheHitKnown: true, CacheHit: false})

	if got := testutil.ToFloat64(collector.CacheMissesTotal); got != 1 {
		t.Fatalf("expected cache misses = 1, got %v", got)
	}
}

func TestProcessEventUnknownCacheStatusDoesNotIncrementCacheCounters(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	collector.ProcessEvent(proxy.QueryEvent{QueryType: "A", ResponseCode: "NOERROR", Upstream: "1.1.1.1:53", CacheHitKnown: false})

	if got := testutil.ToFloat64(collector.CacheHitsTotal); got != 0 {
		t.Fatalf("expected cache hits = 0, got %v", got)
	}
	if got := testutil.ToFloat64(collector.CacheMissesTotal); got != 0 {
		t.Fatalf("expected cache misses = 0, got %v", got)
	}
}

func TestProcessEventNXDomainIncrementsCounter(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	collector.ProcessEvent(proxy.QueryEvent{QueryType: "A", ResponseCode: "NXDOMAIN", Upstream: "1.1.1.1:53"})

	if got := testutil.ToFloat64(collector.NXDomainTotal); got != 1 {
		t.Fatalf("expected nxdomain total = 1, got %v", got)
	}
}

func TestProcessEventServfailIncrementsCounter(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	collector.ProcessEvent(proxy.QueryEvent{QueryType: "A", ResponseCode: "SERVFAIL", Upstream: "1.1.1.1:53"})

	if got := testutil.ToFloat64(collector.ServfailTotal); got != 1 {
		t.Fatalf("expected servfail total = 1, got %v", got)
	}
}

func TestProcessEventBucketsRareQueryTypesIntoOther(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	collector.ProcessEvent(proxy.QueryEvent{QueryType: "NSEC3PARAM", ResponseCode: "NOERROR", Upstream: "1.1.1.1:53"})

	if got := testutil.ToFloat64(collector.QueriesByType.WithLabelValues("OTHER")); got != 1 {
		t.Fatalf("expected OTHER query label count = 1, got %v", got)
	}
}

func TestProcessEventRecordsUpstreamLatencyHistogram(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	collector.ProcessEvent(proxy.QueryEvent{QueryType: "A", ResponseCode: "NOERROR", Upstream: "1.1.1.1:53", LatencyMs: 25})

	mfs, err := collector.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	var found bool
	for _, mf := range mfs {
		if mf.GetName() != "astradns_upstream_latency_seconds" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "upstream" && label.GetValue() == "1.1.1.1:53" {
					if metric.GetHistogram().GetSampleCount() != 1 {
						t.Fatalf("expected histogram sample count = 1, got %d", metric.GetHistogram().GetSampleCount())
					}
					found = true
				}
			}
		}
	}

	if !found {
		t.Fatal("did not find upstream latency metric for upstream label")
	}
}

func TestCollectorRunConsumesEvents(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	events := make(chan proxy.QueryEvent, 1)
	events <- proxy.QueryEvent{QueryType: "A", ResponseCode: "NOERROR", Upstream: "1.1.1.1:53"}
	close(events)

	collector.Run(context.Background(), events)

	if got := testutil.ToFloat64(collector.QueriesTotal); got != 1 {
		t.Fatalf("expected queries total = 1, got %v", got)
	}
}

func TestCollectorRunDrainsBufferedEventsOnCancellation(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	events := make(chan proxy.QueryEvent, 1)
	events <- proxy.QueryEvent{QueryType: "A", ResponseCode: "NOERROR", Upstream: "1.1.1.1:53"}
	close(events)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	collector.Run(ctx, events)

	if got := testutil.ToFloat64(collector.QueriesTotal); got != 1 {
		t.Fatalf("expected queries total = 1 after draining, got %v", got)
	}
}

func TestHandlerExposesExactMetricNames(t *testing.T) {
	collector := NewCollector(prometheus.NewRegistry())
	collector.QueriesByType.WithLabelValues("A").Inc()
	collector.UpstreamQueriesTotal.WithLabelValues("1.1.1.1:53").Inc()
	collector.UpstreamLatencySeconds.WithLabelValues("1.1.1.1:53").Observe(0.01)
	collector.UpstreamFailuresTotal.WithLabelValues("1.1.1.1:53").Inc()
	collector.UpstreamHealthy.WithLabelValues("1.1.1.1:53").Set(1)
	collector.TimeoutTotal.WithLabelValues("1.1.1.1:53").Inc()

	server := httptest.NewServer(collector.Handler())
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("failed to scrape metrics endpoint: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read scrape response: %v", err)
	}
	text := string(body)

	expectedMetricNames := []string{
		"astradns_queries_total",
		"astradns_queries_by_type",
		"astradns_cache_hits_total",
		"astradns_cache_misses_total",
		"astradns_upstream_queries_total",
		"astradns_upstream_latency_seconds",
		"astradns_upstream_failures_total",
		"astradns_upstream_healthy",
		"astradns_nxdomain_total",
		"astradns_servfail_total",
		"astradns_timeout_total",
		"astradns_agent_up",
		"astradns_agent_config_reload_total",
		"astradns_agent_config_reload_errors_total",
		"astradns_agent_engine_recovery_attempts_total",
		"astradns_agent_engine_recovery_success_total",
		"astradns_agent_engine_recovery_errors_total",
		"astradns_proxy_dropped_events_total",
		"astradns_fanout_dropped_events_total",
		"astradns_component_error_buffer_overflows_total",
	}

	for _, metricName := range expectedMetricNames {
		if !strings.Contains(text, metricName) {
			t.Fatalf("expected metric name %q to be present in /metrics output", metricName)
		}
	}
}
