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
