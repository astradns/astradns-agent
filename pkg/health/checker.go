package health

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/astradns/astradns-agent/pkg/metrics"
	"github.com/miekg/dns"
)

// CheckerConfig configures upstream health checking.
type CheckerConfig struct {
	Upstreams        []UpstreamTarget
	IntervalSeconds  int
	TimeoutSeconds   int
	FailureThreshold int
}

// UpstreamTarget is a health check target.
type UpstreamTarget struct {
	Address string
	Port    int
}

// Checker periodically probes upstream DNS resolvers.
type Checker struct {
	config  CheckerConfig
	metrics *metrics.Collector

	mu            sync.RWMutex
	failureCounts map[string]int
	healthy       map[string]bool
}

// NewChecker creates a health checker with sane defaults.
func NewChecker(config CheckerConfig, collector *metrics.Collector) *Checker {
	if config.IntervalSeconds <= 0 {
		config.IntervalSeconds = 10
	}
	if config.TimeoutSeconds <= 0 {
		config.TimeoutSeconds = 2
	}
	if config.FailureThreshold <= 0 {
		config.FailureThreshold = 3
	}

	checker := &Checker{
		config:        config,
		metrics:       collector,
		failureCounts: make(map[string]int),
		healthy:       make(map[string]bool),
	}

	for _, upstream := range checker.config.Upstreams {
		label := upstreamLabel(upstream)
		checker.healthy[label] = false
		if checker.metrics != nil {
			checker.metrics.UpstreamHealthy.WithLabelValues(label).Set(0)
		}
	}

	return checker
}

// Run starts periodic health checks and blocks until the context is canceled.
func (c *Checker) Run(ctx context.Context) {
	c.CheckNow(ctx)

	ticker := time.NewTicker(time.Duration(c.config.IntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkAll(ctx)
		}
	}
}

// CheckNow executes a full health-check pass immediately.
func (c *Checker) CheckNow(ctx context.Context) {
	c.checkAll(ctx)
}

// HasHealthyUpstream reports whether at least one upstream is currently healthy.
func (c *Checker) HasHealthyUpstream() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, healthy := range c.healthy {
		if healthy {
			return true
		}
	}
	return false
}

func (c *Checker) checkAll(ctx context.Context) {
	for _, upstream := range c.config.Upstreams {
		c.checkUpstream(ctx, upstream)
	}
}

func (c *Checker) checkUpstream(ctx context.Context, upstream UpstreamTarget) {
	if upstream.Port == 0 {
		upstream.Port = 53
	}

	timeout := time.Duration(c.config.TimeoutSeconds) * time.Second
	client := &dns.Client{Net: "udp", Timeout: timeout}
	query := new(dns.Msg)
	query.SetQuestion("health.astradns.local.", dns.TypeA)

	label := upstreamLabel(upstream)

	response, rtt, err := client.Exchange(query, label)
	if err != nil || response == nil {
		c.recordFailure(label, isTimeoutErr(err))
		return
	}

	c.recordHealthy(label, rtt)

	if ctx.Err() != nil {
		return
	}
}

func (c *Checker) recordFailure(upstream string, timedOut bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.failureCounts[upstream]++

	if c.metrics != nil {
		c.metrics.UpstreamFailuresTotal.WithLabelValues(upstream).Inc()
		if timedOut {
			c.metrics.TimeoutTotal.WithLabelValues(upstream).Inc()
		}
	}

	if c.failureCounts[upstream] >= c.config.FailureThreshold {
		c.healthy[upstream] = false
		if c.metrics != nil {
			c.metrics.UpstreamHealthy.WithLabelValues(upstream).Set(0)
		}
	}
}

func (c *Checker) recordHealthy(upstream string, latency time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.failureCounts[upstream] = 0
	c.healthy[upstream] = true

	if c.metrics != nil {
		c.metrics.UpstreamHealthy.WithLabelValues(upstream).Set(1)
		c.metrics.UpstreamLatencySeconds.WithLabelValues(upstream).Observe(latency.Seconds())
	}
}

func upstreamLabel(upstream UpstreamTarget) string {
	port := upstream.Port
	if port == 0 {
		port = 53
	}
	return net.JoinHostPort(upstream.Address, strconv.Itoa(port))
}

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
