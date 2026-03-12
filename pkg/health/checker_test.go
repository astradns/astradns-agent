package health

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astradns/astradns-agent/pkg/metrics"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCheckerHealthyUpstreamSetsGaugeToOne(t *testing.T) {
	host, port, responsive, stop := startToggleUpstream(t)
	defer stop()

	responsive.Store(true)

	collector := metrics.NewCollector(prometheus.NewRegistry())
	checker := NewChecker(CheckerConfig{
		Upstreams:        []UpstreamTarget{{Address: host, Port: port}},
		IntervalSeconds:  1,
		TimeoutSeconds:   1,
		FailureThreshold: 2,
	}, collector)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go checker.Run(ctx)

	upstream := fmt.Sprintf("%s:%d", host, port)
	waitFor(t, 3*time.Second, func() bool {
		return testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(upstream)) == 1
	})

	if !checker.HasHealthyUpstream() {
		t.Fatal("expected at least one healthy upstream")
	}
}

func TestCheckerTimeoutSetsGaugeToZeroAfterThreshold(t *testing.T) {
	host, port, responsive, stop := startToggleUpstream(t)
	defer stop()

	responsive.Store(true)

	collector := metrics.NewCollector(prometheus.NewRegistry())
	checker := NewChecker(CheckerConfig{
		Upstreams:        []UpstreamTarget{{Address: host, Port: port}},
		IntervalSeconds:  1,
		TimeoutSeconds:   1,
		FailureThreshold: 2,
	}, collector)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go checker.Run(ctx)

	upstream := fmt.Sprintf("%s:%d", host, port)
	waitFor(t, 3*time.Second, func() bool {
		return testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(upstream)) == 1
	})

	responsive.Store(false)
	waitFor(t, 5*time.Second, func() bool {
		return testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(upstream)) == 0
	})

	if checker.HasHealthyUpstream() {
		t.Fatal("expected no healthy upstreams after repeated timeouts")
	}
}

func TestCheckerRecoverySetsGaugeBackToOne(t *testing.T) {
	host, port, responsive, stop := startToggleUpstream(t)
	defer stop()

	responsive.Store(true)

	collector := metrics.NewCollector(prometheus.NewRegistry())
	checker := NewChecker(CheckerConfig{
		Upstreams:        []UpstreamTarget{{Address: host, Port: port}},
		IntervalSeconds:  1,
		TimeoutSeconds:   1,
		FailureThreshold: 2,
	}, collector)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go checker.Run(ctx)

	upstream := fmt.Sprintf("%s:%d", host, port)
	waitFor(t, 3*time.Second, func() bool {
		return testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(upstream)) == 1
	})

	responsive.Store(false)
	waitFor(t, 5*time.Second, func() bool {
		return testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(upstream)) == 0
	})

	responsive.Store(true)
	waitFor(t, 3*time.Second, func() bool {
		return testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(upstream)) == 1
	})

	if !checker.HasHealthyUpstream() {
		t.Fatal("expected checker to report healthy upstream after recovery")
	}
}

func TestCheckNowPerformsImmediateHealthPass(t *testing.T) {
	host, port, responsive, stop := startToggleUpstream(t)
	defer stop()

	responsive.Store(true)

	collector := metrics.NewCollector(prometheus.NewRegistry())
	checker := NewChecker(CheckerConfig{
		Upstreams:        []UpstreamTarget{{Address: host, Port: port}},
		IntervalSeconds:  30,
		TimeoutSeconds:   1,
		FailureThreshold: 2,
	}, collector)

	checker.CheckNow(context.Background())

	upstream := fmt.Sprintf("%s:%d", host, port)
	if got := testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(upstream)); got != 1 {
		t.Fatalf("expected upstream healthy gauge = 1, got %v", got)
	}
	if !checker.HasHealthyUpstream() {
		t.Fatal("expected checker to report healthy upstream after CheckNow")
	}
}

func TestCheckerFallsBackToTCPWhenUDPUnavailable(t *testing.T) {
	host, port, stop := startTCPOnlyUpstream(t)
	defer stop()

	collector := metrics.NewCollector(prometheus.NewRegistry())
	checker := NewChecker(CheckerConfig{
		Upstreams:        []UpstreamTarget{{Address: host, Port: port}},
		IntervalSeconds:  30,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
	}, collector)

	checker.CheckNow(context.Background())

	upstream := fmt.Sprintf("%s:%d", host, port)
	if got := testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(upstream)); got != 1 {
		t.Fatalf("expected upstream healthy gauge = 1 via TCP fallback, got %v", got)
	}
	if got := testutil.ToFloat64(collector.UpstreamFailuresTotal.WithLabelValues(upstream)); got != 0 {
		t.Fatalf("expected no recorded failure when TCP fallback succeeds, got %v", got)
	}
	if !checker.HasHealthyUpstream() {
		t.Fatal("expected checker to report healthy upstream after TCP fallback")
	}
}

func TestCheckerMultipleUpstreams_OneHealthyOneUnhealthy(t *testing.T) {
	// Start a healthy upstream.
	healthyHost, healthyPort, healthyResponsive, healthyStop := startToggleUpstream(t)
	defer healthyStop()
	healthyResponsive.Store(true)

	// Start an unhealthy upstream (never responds).
	unhealthyHost, unhealthyPort, unhealthyResponsive, unhealthyStop := startToggleUpstream(t)
	defer unhealthyStop()
	unhealthyResponsive.Store(false)

	collector := metrics.NewCollector(prometheus.NewRegistry())
	checker := NewChecker(CheckerConfig{
		Upstreams: []UpstreamTarget{
			{Address: healthyHost, Port: healthyPort},
			{Address: unhealthyHost, Port: unhealthyPort},
		},
		IntervalSeconds:  30,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
	}, collector)

	checker.CheckNow(context.Background())

	if !checker.HasHealthyUpstream() {
		t.Fatal("expected HasHealthyUpstream to be true when one upstream is healthy")
	}

	healthyLabel := fmt.Sprintf("%s:%d", healthyHost, healthyPort)
	if got := testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(healthyLabel)); got != 1 {
		t.Fatalf("expected healthy upstream gauge = 1, got %v", got)
	}
}

func TestCheckerMultipleUpstreams_BothUnhealthy(t *testing.T) {
	host1, port1, responsive1, stop1 := startToggleUpstream(t)
	defer stop1()
	responsive1.Store(false)

	host2, port2, responsive2, stop2 := startToggleUpstream(t)
	defer stop2()
	responsive2.Store(false)

	collector := metrics.NewCollector(prometheus.NewRegistry())
	checker := NewChecker(CheckerConfig{
		Upstreams: []UpstreamTarget{
			{Address: host1, Port: port1},
			{Address: host2, Port: port2},
		},
		IntervalSeconds:  30,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
	}, collector)

	checker.CheckNow(context.Background())

	if checker.HasHealthyUpstream() {
		t.Fatal("expected HasHealthyUpstream to be false when both upstreams are unhealthy")
	}
}

func TestCheckerPort0NormalizesTo53(t *testing.T) {
	// Verify that upstreamLabel normalizes port 0 to port 53.
	target := UpstreamTarget{Address: "10.0.0.1", Port: 0}
	label := upstreamLabel(target)
	expected := "10.0.0.1:53"
	if label != expected {
		t.Fatalf("expected label %q for port 0, got %q", expected, label)
	}
}

func TestCheckerContextCancellationDuringHealthCheck(t *testing.T) {
	// Start an upstream that never responds to simulate a slow health check.
	host, port, responsive, stop := startToggleUpstream(t)
	defer stop()
	responsive.Store(false)

	collector := metrics.NewCollector(prometheus.NewRegistry())
	checker := NewChecker(CheckerConfig{
		Upstreams:        []UpstreamTarget{{Address: host, Port: port}},
		IntervalSeconds:  1,
		TimeoutSeconds:   2,
		FailureThreshold: 3,
	}, collector)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		checker.Run(ctx)
		close(done)
	}()

	// Cancel the context quickly.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Run returned as expected after context cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("checker.Run did not return after context cancellation")
	}
}

func TestCheckerUpdateUpstreamsReplacesTargets(t *testing.T) {
	oldHost, oldPort, oldResponsive, oldStop := startToggleUpstream(t)
	defer oldStop()
	oldResponsive.Store(true)

	newHost, newPort, newResponsive, newStop := startToggleUpstream(t)
	defer newStop()
	newResponsive.Store(false)

	collector := metrics.NewCollector(prometheus.NewRegistry())
	checker := NewChecker(CheckerConfig{
		Upstreams:        []UpstreamTarget{{Address: oldHost, Port: oldPort}},
		IntervalSeconds:  30,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
	}, collector)

	checker.CheckNow(context.Background())

	oldLabel := fmt.Sprintf("%s:%d", oldHost, oldPort)
	if got := testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(oldLabel)); got != 1 {
		t.Fatalf("expected old upstream gauge = 1 before update, got %v", got)
	}
	if !checker.HasHealthyUpstream() {
		t.Fatal("expected checker to report healthy upstream before update")
	}

	checker.UpdateUpstreams([]UpstreamTarget{{Address: newHost, Port: newPort}})

	if got := testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(oldLabel)); got != 0 {
		t.Fatalf("expected old upstream gauge = 0 after update, got %v", got)
	}
	if checker.HasHealthyUpstream() {
		t.Fatal("expected checker to report no healthy upstreams immediately after replacing targets")
	}

	newResponsive.Store(true)
	checker.CheckNow(context.Background())

	newLabel := fmt.Sprintf("%s:%d", newHost, newPort)
	if got := testutil.ToFloat64(collector.UpstreamHealthy.WithLabelValues(newLabel)); got != 1 {
		t.Fatalf("expected new upstream gauge = 1 after check, got %v", got)
	}
	if !checker.HasHealthyUpstream() {
		t.Fatal("expected checker to report healthy upstream after update + check")
	}
}

func startToggleUpstream(t *testing.T) (string, int, *atomic.Bool, func()) {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test upstream: %v", err)
	}

	addr := conn.LocalAddr().(*net.UDPAddr)
	responsive := &atomic.Bool{}

	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		if !responsive.Load() {
			return
		}
		response := new(dns.Msg)
		response.SetReply(r)
		record, recordErr := dns.NewRR(fmt.Sprintf("%s 60 IN A 127.0.0.1", r.Question[0].Name))
		if recordErr == nil {
			response.Answer = append(response.Answer, record)
		}
		_ = w.WriteMsg(response)
	})

	server := &dns.Server{PacketConn: conn, Net: "udp", Handler: handler}
	go func() {
		_ = server.ActivateAndServe()
	}()

	stop := func() {
		_ = server.Shutdown()
	}

	return addr.IP.String(), addr.Port, responsive, stop
}

func startTCPOnlyUpstream(t *testing.T) (string, int, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test tcp upstream: %v", err)
	}

	addr := listener.Addr().(*net.TCPAddr)
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(r)
		record, recordErr := dns.NewRR(fmt.Sprintf("%s 60 IN A 127.0.0.1", r.Question[0].Name))
		if recordErr == nil {
			response.Answer = append(response.Answer, record)
		}
		_ = w.WriteMsg(response)
	})

	server := &dns.Server{Listener: listener, Net: "tcp", Handler: handler}
	go func() {
		_ = server.ActivateAndServe()
	}()

	stop := func() {
		_ = server.Shutdown()
	}

	return addr.IP.String(), addr.Port, stop
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("condition was not met before timeout")
}
