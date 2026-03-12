//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/astradns/astradns-agent/pkg/metrics"
	"github.com/astradns/astradns-agent/pkg/proxy"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestProxyToEngineFlowUpdatesMetrics(t *testing.T) {
	engineAddr, stopEngine := startMockEngine(t)
	defer stopEngine()

	listenAddr := reserveLocalAddr(t)
	proxyInstance := proxy.New(proxy.ProxyConfig{
		ListenAddr:    listenAddr,
		EngineAddr:    engineAddr,
		EventChanSize: 64,
	})

	collector := metrics.NewCollector(prometheus.NewRegistry())
	collectorEvents := make(chan proxy.QueryEvent, 64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxyErr := make(chan error, 1)
	go func() {
		proxyErr <- proxyInstance.Start(ctx)
	}()

	collectorDone := make(chan struct{})
	go func() {
		collector.Run(ctx, collectorEvents)
		close(collectorDone)
	}()

	forwardDone := make(chan struct{})
	go func() {
		for event := range proxyInstance.Events() {
			collectorEvents <- event
		}
		close(collectorEvents)
		close(forwardDone)
	}()

	response := queryWithRetry(t, listenAddr, "example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR response, got rcode %d", response.Rcode)
	}

	if len(response.Answer) == 0 {
		t.Fatal("expected at least one DNS answer record")
	}

	waitForMetric(t, 3*time.Second, func() float64 { return testutil.ToFloat64(collector.QueriesTotal) }, 1)
	waitForMetric(
		t,
		3*time.Second,
		func() float64 { return testutil.ToFloat64(collector.UpstreamQueriesTotal.WithLabelValues(engineAddr)) },
		1,
	)

	cancel()

	select {
	case err := <-proxyErr:
		if err != nil {
			t.Fatalf("proxy returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not stop after cancellation")
	}

	select {
	case <-forwardDone:
	case <-time.After(3 * time.Second):
		t.Fatal("event forwarder did not stop")
	}

	select {
	case <-collectorDone:
	case <-time.After(3 * time.Second):
		t.Fatal("collector did not stop")
	}
}

func startMockEngine(t *testing.T) (string, func()) {
	t.Helper()

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create tcp listener: %v", err)
	}

	addr := tcpListener.Addr().(*net.TCPAddr)
	udpConn, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", addr.Port))
	if err != nil {
		_ = tcpListener.Close()
		t.Fatalf("failed to create udp listener: %v", err)
	}

	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(r)
		if len(r.Question) > 0 && r.Question[0].Qtype == dns.TypeA {
			record, recordErr := dns.NewRR(fmt.Sprintf("%s 60 IN A 203.0.113.42", r.Question[0].Name))
			if recordErr == nil {
				response.Answer = append(response.Answer, record)
			}
		}
		_ = w.WriteMsg(response)
	})

	udpServer := &dns.Server{PacketConn: udpConn, Net: "udp", Handler: handler}
	tcpServer := &dns.Server{Listener: tcpListener, Net: "tcp", Handler: handler}

	go func() {
		_ = udpServer.ActivateAndServe()
	}()
	go func() {
		_ = tcpServer.ActivateAndServe()
	}()

	stop := func() {
		_ = udpServer.Shutdown()
		_ = tcpServer.Shutdown()
	}

	return tcpListener.Addr().String(), stop
}

func reserveLocalAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve local address: %v", err)
	}
	defer listener.Close()

	return listener.Addr().String()
}

func queryWithRetry(t *testing.T, addr, domain string) *dns.Msg {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		query := new(dns.Msg)
		query.SetQuestion(domain, dns.TypeA)

		client := &dns.Client{Net: "udp", Timeout: 300 * time.Millisecond}
		response, _, err := client.Exchange(query, addr)
		if err == nil {
			return response
		}

		if time.Now().After(deadline) {
			t.Fatalf("query failed after retries: %v", err)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func waitForMetric(t *testing.T, timeout time.Duration, value func() float64, expected float64) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if value() >= expected {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for metric to reach %.0f", expected)
}
