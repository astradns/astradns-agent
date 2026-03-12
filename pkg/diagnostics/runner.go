package diagnostics

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	defaultResolverAddr = "127.0.0.1:5353"
	defaultInterval     = time.Minute
	defaultTimeout      = 3 * time.Second
)

// Diagnosis classifies the most likely failure domain for a target probe.
type Diagnosis string

const (
	DiagnosisHealthy                Diagnosis = "healthy"
	DiagnosisDNSResolutionFailed    Diagnosis = "dns_resolution_failed"
	DiagnosisEgressBlockedOrNetwork Diagnosis = "egress_blocked_or_network"
	DiagnosisTLSHandshakeFailed     Diagnosis = "tls_handshake_failed"
	DiagnosisTLSSNIRequired         Diagnosis = "tls_sni_required"
	DiagnosisProviderBlockSuspected Diagnosis = "provider_block_suspected"
)

// Config configures the diagnostics runner.
type Config struct {
	Targets      []string
	ResolverAddr string
	Interval     time.Duration
	Timeout      time.Duration
}

// Result is the latest diagnosis for a target.
type Result struct {
	Target              string    `json:"target"`
	ObservedAt          time.Time `json:"observedAt"`
	Diagnosis           Diagnosis `json:"diagnosis"`
	Summary             string    `json:"summary"`
	Resolver            string    `json:"resolver"`
	DNSRcode            string    `json:"dnsRcode,omitempty"`
	DNSLatencyMs        float64   `json:"dnsLatencyMs,omitempty"`
	ResolvedIPs         []string  `json:"resolvedIPs,omitempty"`
	TestedEndpoint      string    `json:"testedEndpoint,omitempty"`
	TCPReachable        bool      `json:"tcpReachable"`
	TCPConnectLatencyMs float64   `json:"tcpConnectLatencyMs,omitempty"`
	TLSWithSNI          bool      `json:"tlsWithSNI"`
	TLSWithSNILatencyMs float64   `json:"tlsWithSNILatencyMs,omitempty"`
	TLSWithoutSNI       bool      `json:"tlsWithoutSNI"`
	HTTPSStatusCode     int       `json:"httpsStatusCode,omitempty"`
	Error               string    `json:"error,omitempty"`
	Evidence            []string  `json:"evidence,omitempty"`
}

// Snapshot captures current diagnostic state for all targets.
type Snapshot struct {
	ObservedAt time.Time `json:"observedAt"`
	Results    []Result  `json:"results"`
}

// Runner executes periodic diagnostics probes and stores latest results.
type Runner struct {
	cfg    Config
	logger *slog.Logger
	now    func() time.Time

	mu          sync.RWMutex
	results     map[string]Result
	lastUpdated time.Time
}

// NewRunner creates a diagnostics runner with sane defaults.
func NewRunner(cfg Config, logger *slog.Logger) *Runner {
	if strings.TrimSpace(cfg.ResolverAddr) == "" {
		cfg.ResolverAddr = defaultResolverAddr
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}

	normalizedTargets := make([]string, 0, len(cfg.Targets))
	seen := make(map[string]struct{}, len(cfg.Targets))
	for _, target := range cfg.Targets {
		normalized := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(target)), ".")
		if normalized == "" || !isTargetHostname(normalized) {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		normalizedTargets = append(normalizedTargets, normalized)
	}
	sort.Strings(normalizedTargets)
	cfg.Targets = normalizedTargets

	if logger == nil {
		logger = slog.Default()
	}

	return &Runner{
		cfg:     cfg,
		logger:  logger,
		now:     time.Now,
		results: make(map[string]Result, len(cfg.Targets)),
	}
}

// Run starts periodic diagnostics checks until context cancellation.
func (r *Runner) Run(ctx context.Context) {
	if len(r.cfg.Targets) == 0 {
		return
	}

	r.runOnce(ctx)

	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

// Snapshot returns a copy of the latest diagnostic report.
func (r *Runner) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	results := make([]Result, 0, len(r.results))
	for _, result := range r.results {
		copyResult := result
		copyResult.ResolvedIPs = append([]string(nil), result.ResolvedIPs...)
		copyResult.Evidence = append([]string(nil), result.Evidence...)
		results = append(results, copyResult)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Target < results[j].Target
	})

	return Snapshot{ObservedAt: r.lastUpdated, Results: results}
}

func (r *Runner) runOnce(ctx context.Context) {
	updatedAt := r.now()
	for _, target := range r.cfg.Targets {
		if ctx.Err() != nil {
			return
		}

		probeCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout*3)
		result := r.probeTarget(probeCtx, target)
		cancel()

		r.mu.Lock()
		r.results[target] = result
		r.lastUpdated = updatedAt
		r.mu.Unlock()

		if result.Diagnosis != DiagnosisHealthy {
			r.logger.Warn(
				"diagnostics detected issue",
				"target", result.Target,
				"diagnosis", result.Diagnosis,
				"summary", result.Summary,
				"error", result.Error,
			)
		}
	}
}

func (r *Runner) probeTarget(ctx context.Context, target string) Result {
	result := Result{
		Target:     target,
		ObservedAt: r.now(),
		Resolver:   r.cfg.ResolverAddr,
	}

	outcome := probeOutcome{}

	ips, rcode, latency, err := r.resolveTarget(ctx, target)
	result.DNSRcode = rcode
	result.DNSLatencyMs = milliseconds(latency)
	result.ResolvedIPs = append(result.ResolvedIPs, ips...)
	outcome.dnsRcode = rcode
	outcome.resolvedIPs = ips
	outcome.dnsErr = err

	if err == nil && len(ips) > 0 {
		endpoint, tcpLatency, tcpErr := r.probeTCPReachability(ctx, ips)
		result.TCPConnectLatencyMs = milliseconds(tcpLatency)
		if tcpErr == nil {
			result.TCPReachable = true
			result.TestedEndpoint = endpoint
			outcome.tcpReachable = true
		} else {
			outcome.tcpErr = tcpErr
		}

		if outcome.tcpReachable {
			result.TLSWithSNI, latency, outcome.tlsWithSNIErr = r.probeTLS(ctx, endpoint, target, true)
			result.TLSWithSNILatencyMs = milliseconds(latency)

			result.TLSWithoutSNI, _, outcome.tlsWithoutSNIErr = r.probeTLS(ctx, endpoint, target, false)

			if result.TLSWithSNI {
				statusCode, httpErr := r.probeHTTPSStatus(ctx, target, endpoint)
				result.HTTPSStatusCode = statusCode
				outcome.httpStatusCode = statusCode
				outcome.httpErr = httpErr
			}
		}
	}

	result.Diagnosis, result.Summary, result.Error, result.Evidence = classifyOutcome(outcome)
	return result
}

func (r *Runner) resolveTarget(ctx context.Context, target string) ([]string, string, time.Duration, error) {
	client := &dns.Client{Timeout: r.cfg.Timeout}

	ips := make(map[string]struct{})
	queryTypes := []uint16{dns.TypeA, dns.TypeAAAA}

	var rcode string
	var latency time.Duration
	var firstErr error

	for index, queryType := range queryTypes {
		message := new(dns.Msg)
		message.SetQuestion(dns.Fqdn(target), queryType)

		response, rtt, err := client.ExchangeContext(ctx, message, r.cfg.ResolverAddr)
		if index == 0 {
			latency = rtt
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		if index == 0 {
			rcode = dns.RcodeToString[response.Rcode]
		}

		if response.Rcode != dns.RcodeSuccess {
			if index == 0 {
				return nil, dns.RcodeToString[response.Rcode], latency,
					fmt.Errorf("dns returned rcode %s", dns.RcodeToString[response.Rcode])
			}
			continue
		}

		for _, answer := range response.Answer {
			switch record := answer.(type) {
			case *dns.A:
				ips[record.A.String()] = struct{}{}
			case *dns.AAAA:
				ips[record.AAAA.String()] = struct{}{}
			}
		}
	}

	if len(ips) == 0 {
		if firstErr != nil {
			return nil, rcode, latency, firstErr
		}
		return nil, rcode, latency, errors.New("dns query succeeded but no A/AAAA records were returned")
	}

	resolved := make([]string, 0, len(ips))
	for ip := range ips {
		resolved = append(resolved, ip)
	}
	sort.Strings(resolved)

	if rcode == "" {
		rcode = dns.RcodeToString[dns.RcodeSuccess]
	}

	return resolved, rcode, latency, nil
}

func (r *Runner) probeTCPReachability(ctx context.Context, ips []string) (string, time.Duration, error) {
	dialer := &net.Dialer{Timeout: r.cfg.Timeout}

	errorsByEndpoint := make([]string, 0, len(ips))
	for _, ip := range ips {
		endpoint := net.JoinHostPort(ip, "443")
		start := time.Now()
		connection, err := dialer.DialContext(ctx, "tcp", endpoint)
		latency := time.Since(start)
		if err != nil {
			errorsByEndpoint = append(errorsByEndpoint, fmt.Sprintf("%s: %v", endpoint, err))
			continue
		}
		_ = connection.Close()
		return endpoint, latency, nil
	}

	if len(errorsByEndpoint) == 0 {
		return "", 0, errors.New("no resolved ip addresses available for tcp probe")
	}

	return "", 0, fmt.Errorf("tcp probe failed for all resolved endpoints: %s", strings.Join(errorsByEndpoint, "; "))
}

func (r *Runner) probeTLS(
	ctx context.Context,
	endpoint string,
	target string,
	withSNI bool,
) (bool, time.Duration, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if withSNI {
		tlsConfig.ServerName = target
	}

	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: r.cfg.Timeout},
		Config:    tlsConfig,
	}

	start := time.Now()
	connection, err := dialer.DialContext(ctx, "tcp", endpoint)
	latency := time.Since(start)
	if err != nil {
		return false, latency, err
	}

	_ = connection.Close()
	return true, latency, nil
}

func (r *Runner) probeHTTPSStatus(ctx context.Context, target string, endpoint string) (int, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: target},
		DialContext: func(ctx context.Context, network string, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: r.cfg.Timeout}).DialContext(ctx, network, endpoint)
		},
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: r.cfg.Timeout}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+target+"/", nil)
	if err != nil {
		return 0, err
	}

	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	return response.StatusCode, nil
}

type probeOutcome struct {
	dnsErr           error
	dnsRcode         string
	resolvedIPs      []string
	tcpReachable     bool
	tcpErr           error
	tlsWithSNIErr    error
	tlsWithoutSNIErr error
	httpStatusCode   int
	httpErr          error
}

func classifyOutcome(outcome probeOutcome) (Diagnosis, string, string, []string) {
	evidence := make([]string, 0, 6)
	if outcome.dnsRcode != "" {
		evidence = append(evidence, "dns_rcode="+outcome.dnsRcode)
	}
	if len(outcome.resolvedIPs) > 0 {
		evidence = append(evidence, "resolved_ips="+strings.Join(outcome.resolvedIPs, ","))
	}
	evidence = append(evidence, fmt.Sprintf("tcp_reachable=%t", outcome.tcpReachable))

	if outcome.dnsErr != nil || len(outcome.resolvedIPs) == 0 {
		errText := errorText(outcome.dnsErr)
		return DiagnosisDNSResolutionFailed,
			"local DNS resolution failed before any network probe",
			errText,
			evidence
	}

	if !outcome.tcpReachable {
		errText := errorText(outcome.tcpErr)
		if errText != "" {
			evidence = append(evidence, "tcp_error="+errText)
		}
		return DiagnosisEgressBlockedOrNetwork,
			"DNS resolved but TCP/443 reachability failed for all resolved endpoints",
			errText,
			evidence
	}

	if outcome.tlsWithSNIErr != nil {
		errText := errorText(outcome.tlsWithSNIErr)
		evidence = append(evidence, "tls_sni_error="+errText)
		return DiagnosisTLSHandshakeFailed,
			"TCP/443 is reachable but TLS handshake with SNI failed",
			errText,
			evidence
	}

	if outcome.tlsWithoutSNIErr != nil {
		evidence = append(evidence, "tls_without_sni_error="+errorText(outcome.tlsWithoutSNIErr))
		return DiagnosisTLSSNIRequired,
			"TLS succeeds with SNI and fails without SNI, indicating SNI-sensitive endpoint behavior",
			"",
			evidence
	}

	if outcome.httpStatusCode == http.StatusForbidden ||
		outcome.httpStatusCode == http.StatusTooManyRequests ||
		outcome.httpStatusCode == http.StatusUnavailableForLegalReasons {
		evidence = append(evidence, fmt.Sprintf("https_status=%d", outcome.httpStatusCode))
		if outcome.httpErr != nil {
			evidence = append(evidence, "https_error="+errorText(outcome.httpErr))
		}
		return DiagnosisProviderBlockSuspected,
			"network and TLS are healthy, but HTTPS status suggests potential provider-side access policy",
			"",
			evidence
	}

	if outcome.httpStatusCode > 0 {
		evidence = append(evidence, fmt.Sprintf("https_status=%d", outcome.httpStatusCode))
	}
	if outcome.httpErr != nil {
		evidence = append(evidence, "https_error="+errorText(outcome.httpErr))
	}

	return DiagnosisHealthy,
		"DNS, TCP, and TLS probes succeeded for this target",
		"",
		evidence
}

func errorText(err error) string {
	if err == nil {
		return ""
	}

	return strings.TrimSpace(err.Error())
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func isTargetHostname(value string) bool {
	if value == "" {
		return false
	}
	if net.ParseIP(value) != nil {
		return true
	}
	if len(value) > 253 {
		return false
	}

	labels := strings.Split(value, ".")
	for _, label := range labels {
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
	}

	return true
}
