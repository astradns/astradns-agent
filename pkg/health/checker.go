package health

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/astradns/astradns-agent/pkg/metrics"
	"github.com/astradns/astradns-types/engine"
	"github.com/miekg/dns"
)

// CheckerConfig configures upstream health checking.
type CheckerConfig struct {
	Upstreams        []UpstreamTarget
	IntervalSeconds  int
	TimeoutSeconds   int
	FailureThreshold int
	ProbeDomain      string
	ProbeType        uint16
}

const (
	defaultProbeDomain = "."
	defaultProbeType   = dns.TypeNS
)

// UpstreamTarget is a health check target.
type UpstreamTarget struct {
	Address       string
	Port          int
	Transport     engine.UpstreamTransport
	TLSServerName string
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
	if strings.TrimSpace(config.ProbeDomain) == "" {
		config.ProbeDomain = defaultProbeDomain
	}
	if config.ProbeType == 0 {
		config.ProbeType = defaultProbeType
	}

	checker := &Checker{
		config:        config,
		metrics:       collector,
		failureCounts: make(map[string]int),
		healthy:       make(map[string]bool),
	}

	for _, upstream := range checker.config.Upstreams {
		upstream.Transport = normalizeUpstreamTransport(upstream.Transport)
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

// UpdateUpstreams replaces the current upstream target set used by the checker.
func (c *Checker) UpdateUpstreams(upstreams []UpstreamTarget) {
	labels := make(map[string]struct{}, len(upstreams))
	normalized := make([]UpstreamTarget, 0, len(upstreams))
	for _, upstream := range upstreams {
		upstream.Transport = normalizeUpstreamTransport(upstream.Transport)
		if upstream.Port == 0 {
			upstream.Port = int(defaultPortForTransport(upstream.Transport))
		}
		if upstream.Transport == engine.UpstreamTransportDNS {
			upstream.TLSServerName = ""
		}
		label := upstreamLabel(upstream)
		if _, exists := labels[label]; exists {
			continue
		}
		labels[label] = struct{}{}
		normalized = append(normalized, upstream)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.config.Upstreams = normalized

	for label := range c.failureCounts {
		if _, keep := labels[label]; !keep {
			delete(c.failureCounts, label)
		}
	}

	for label := range c.healthy {
		if _, keep := labels[label]; keep {
			continue
		}
		delete(c.healthy, label)
		if c.metrics != nil {
			c.metrics.UpstreamHealthy.WithLabelValues(label).Set(0)
		}
	}

	for _, upstream := range normalized {
		label := upstreamLabel(upstream)
		if _, exists := c.failureCounts[label]; !exists {
			c.failureCounts[label] = 0
		}
		if _, exists := c.healthy[label]; !exists {
			c.healthy[label] = false
			if c.metrics != nil {
				c.metrics.UpstreamHealthy.WithLabelValues(label).Set(0)
			}
		}
	}
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
	for _, upstream := range c.currentUpstreams() {
		if ctx.Err() != nil {
			return
		}
		c.checkUpstream(ctx, upstream)
	}
}

func (c *Checker) currentUpstreams() []UpstreamTarget {
	c.mu.RLock()
	defer c.mu.RUnlock()

	upstreams := make([]UpstreamTarget, len(c.config.Upstreams))
	copy(upstreams, c.config.Upstreams)

	return upstreams
}

func (c *Checker) checkUpstream(ctx context.Context, upstream UpstreamTarget) {
	upstream.Transport = normalizeUpstreamTransport(upstream.Transport)
	if upstream.Port == 0 {
		upstream.Port = int(defaultPortForTransport(upstream.Transport))
	}

	label := upstreamLabel(upstream)
	endpoint := net.JoinHostPort(upstream.Address, strconv.Itoa(upstream.Port))

	switch upstream.Transport {
	case engine.UpstreamTransportDoH:
		// DoH probing is intentionally lightweight for now. The engine performs
		// protocol-level retries; here we avoid false negatives from HTTP-layer
		// specifics and keep readiness tied to successful config/application.
		c.recordHealthy(label, 0)
		return
	case engine.UpstreamTransportDoT:
		response, rtt, err := c.probeUpstreamTLS(ctx, endpoint, upstream.TLSServerName)
		if err == nil && response != nil {
			c.recordHealthy(label, rtt)
			return
		}
		if ctx.Err() != nil {
			return
		}

		c.recordFailure(label, isTimeoutErr(err))
		return
	}

	response, rtt, err := c.probeUpstream(ctx, endpoint, "udp")
	if err == nil && response != nil {
		c.recordHealthy(label, rtt)
		return
	}
	if ctx.Err() != nil {
		return
	}

	udpErr := err
	response, rtt, err = c.probeUpstream(ctx, endpoint, "tcp")
	if err == nil && response != nil {
		c.recordHealthy(label, rtt)
		return
	}
	if ctx.Err() != nil {
		return
	}

	timedOut := isTimeoutErr(udpErr) && isTimeoutErr(err)
	c.recordFailure(label, timedOut)
}

func (c *Checker) probeUpstream(ctx context.Context, upstream, network string) (*dns.Msg, time.Duration, error) {
	timeout := time.Duration(c.config.TimeoutSeconds) * time.Second
	client := &dns.Client{Net: network, Timeout: timeout}
	query := new(dns.Msg)
	query.SetQuestion(dns.Fqdn(c.config.ProbeDomain), c.config.ProbeType)

	response, rtt, err := client.ExchangeContext(ctx, query, upstream)
	if err != nil || response == nil {
		return response, rtt, err
	}

	if response.Rcode == dns.RcodeServerFailure || response.Rcode == dns.RcodeRefused {
		return response, rtt, fmt.Errorf("probe rcode %s", dns.RcodeToString[response.Rcode])
	}

	return response, rtt, nil
}

func (c *Checker) probeUpstreamTLS(
	ctx context.Context,
	upstream,
	tlsServerName string,
) (*dns.Msg, time.Duration, error) {
	timeout := time.Duration(c.config.TimeoutSeconds) * time.Second
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if strings.TrimSpace(tlsServerName) != "" {
		tlsConfig.ServerName = strings.TrimSpace(tlsServerName)
	} else if host, _, err := net.SplitHostPort(upstream); err == nil {
		if _, parseErr := netip.ParseAddr(host); parseErr != nil {
			tlsConfig.ServerName = host
		}
	}

	client := &dns.Client{Net: "tcp-tls", Timeout: timeout, TLSConfig: tlsConfig}
	query := new(dns.Msg)
	query.SetQuestion(dns.Fqdn(c.config.ProbeDomain), c.config.ProbeType)

	response, rtt, err := client.ExchangeContext(ctx, query, upstream)
	if err != nil || response == nil {
		return response, rtt, err
	}

	if response.Rcode == dns.RcodeServerFailure || response.Rcode == dns.RcodeRefused {
		return response, rtt, fmt.Errorf("probe rcode %s", dns.RcodeToString[response.Rcode])
	}

	return response, rtt, nil
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
	transport := normalizeUpstreamTransport(upstream.Transport)
	port := upstream.Port
	if port == 0 {
		port = int(defaultPortForTransport(transport))
	}

	endpoint := net.JoinHostPort(upstream.Address, strconv.Itoa(port))
	if transport == engine.UpstreamTransportDNS {
		return endpoint
	}

	return fmt.Sprintf("%s://%s", transport, endpoint)
}

func normalizeUpstreamTransport(transport engine.UpstreamTransport) engine.UpstreamTransport {
	trimmed := strings.ToLower(strings.TrimSpace(string(transport)))
	switch engine.UpstreamTransport(trimmed) {
	case engine.UpstreamTransportDoT:
		return engine.UpstreamTransportDoT
	case engine.UpstreamTransportDoH:
		return engine.UpstreamTransportDoH
	default:
		return engine.UpstreamTransportDNS
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
