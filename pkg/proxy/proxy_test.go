package proxy

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestProxyForwardsQueriesAndEmitsEvents(t *testing.T) {
	engineAddr, stopEngine := startTestEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{ListenAddr: listenAddr, EngineAddr: engineAddr, EventChanSize: 8})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	response := exchangeWithRetry(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR response, got rcode %d", response.Rcode)
	}

	select {
	case event := <-p.Events():
		if event.Domain != "example.org" {
			t.Fatalf("expected domain example.org, got %q", event.Domain)
		}
		if event.QueryType != "A" {
			t.Fatalf("expected query type A, got %q", event.QueryType)
		}
		if event.ResponseCode != "NOERROR" {
			t.Fatalf("expected NOERROR, got %q", event.ResponseCode)
		}
		if event.SourceIP == "" {
			t.Fatal("expected source IP to be set")
		}
		if event.Upstream != engineAddr {
			t.Fatalf("expected upstream %q, got %q", engineAddr, event.Upstream)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for query event")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected proxy start error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after context cancellation")
	}
}

func TestProxyCachesResponsesAndMarksCacheHits(t *testing.T) {
	engineAddr, upstreamQueries, stopEngine := startCountingEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{
		ListenAddr:      listenAddr,
		EngineAddr:      engineAddr,
		EventChanSize:   8,
		CacheMaxEntries: 16,
		CacheDefaultTTL: 30 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	firstResponse := exchangeWithRetry(t, "udp", listenAddr, "cache.example.org.")
	if firstResponse.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected first NOERROR response, got rcode %d", firstResponse.Rcode)
	}

	secondResponse := exchangeWithRetry(t, "udp", listenAddr, "cache.example.org.")
	if secondResponse.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected second NOERROR response, got rcode %d", secondResponse.Rcode)
	}

	if got := upstreamQueries.Load(); got != 1 {
		t.Fatalf("expected one upstream query due to cache hit, got %d", got)
	}

	firstEvent := awaitEvent(t, p.Events())
	if !firstEvent.CacheHitKnown {
		t.Fatal("expected first event to report known cache status")
	}
	if firstEvent.CacheHit {
		t.Fatal("expected first event to be cache miss")
	}
	if firstEvent.Upstream != engineAddr {
		t.Fatalf("expected first event upstream %q, got %q", engineAddr, firstEvent.Upstream)
	}

	secondEvent := awaitEvent(t, p.Events())
	if !secondEvent.CacheHitKnown {
		t.Fatal("expected second event to report known cache status")
	}
	if !secondEvent.CacheHit {
		t.Fatal("expected second event to be cache hit")
	}
	if secondEvent.Upstream != cacheHitUpstreamLabel {
		t.Fatalf("expected second event upstream %q, got %q", cacheHitUpstreamLabel, secondEvent.Upstream)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected proxy start error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after context cancellation")
	}
}

func TestProxyReusesEngineConnections(t *testing.T) {
	engineAddr, upstreamQueries, stopEngine := startCountingEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{
		ListenAddr:               listenAddr,
		EngineAddr:               engineAddr,
		EventChanSize:            8,
		EngineConnectionPoolSize: 4,
	})

	originalDial := p.pool.dial
	var dialCount atomic.Int64
	p.pool.dial = func(network, address string, timeout time.Duration) (*dns.Conn, error) {
		dialCount.Add(1)
		return originalDial(network, address, timeout)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	for i := 0; i < 5; i++ {
		response := exchangeWithRetry(t, "udp", listenAddr, fmt.Sprintf("reuse-%d.example.org.", i))
		if response.Rcode != dns.RcodeSuccess {
			t.Fatalf("expected NOERROR response, got rcode %d", response.Rcode)
		}
	}

	if got := upstreamQueries.Load(); got != 5 {
		t.Fatalf("expected 5 upstream queries, got %d", got)
	}

	if got := dialCount.Load(); got > 2 {
		t.Fatalf("expected pooled engine dials to stay low, got %d", got)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected proxy start error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after context cancellation")
	}
}

func TestProxyDropsEventsWhenChannelIsFull(t *testing.T) {
	engineAddr, stopEngine := startTestEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	var callbackDrops atomic.Int64
	p := New(ProxyConfig{
		ListenAddr:    listenAddr,
		EngineAddr:    engineAddr,
		EventChanSize: 1,
		OnEventDrop: func() {
			callbackDrops.Add(1)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	response := exchangeWithRetry(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR response, got rcode %d", response.Rcode)
	}

	response = exchangeWithRetry(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR response, got rcode %d", response.Rcode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for p.DroppedEvents() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if p.DroppedEvents() == 0 {
		t.Fatal("expected dropped events counter to increase")
	}
	if callbackDrops.Load() == 0 {
		t.Fatal("expected drop callback to be invoked")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected proxy start error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after context cancellation")
	}
}

func TestProxySupportsTCPQueries(t *testing.T) {
	engineAddr, stopEngine := startTestEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{ListenAddr: listenAddr, EngineAddr: engineAddr, EventChanSize: 8})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	response := exchangeWithRetry(t, "tcp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR response, got rcode %d", response.Rcode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected proxy start error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after context cancellation")
	}
}

func TestProxyRateLimitsPerSourceQueries(t *testing.T) {
	engineAddr, stopEngine := startTestEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{
		ListenAddr:                   listenAddr,
		EngineAddr:                   engineAddr,
		EventChanSize:                8,
		GlobalRateLimitRPS:           1000,
		GlobalRateLimitBurst:         1000,
		PerSourceRateLimitRPS:        1,
		PerSourceRateLimitBurst:      1,
		PerSourceRateLimitStateTTL:   time.Minute,
		PerSourceRateLimitMaxSources: 100,
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	response := exchangeWithRetry(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR response, got rcode %d", response.Rcode)
	}

	response = exchangeWithRetry(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeRefused {
		t.Fatalf("expected REFUSED response due to rate limit, got rcode %d", response.Rcode)
	}

	first := awaitEvent(t, p.Events())
	if first.ResponseCode != "NOERROR" {
		t.Fatalf("expected first event response code NOERROR, got %q", first.ResponseCode)
	}

	second := awaitEvent(t, p.Events())
	if second.ResponseCode != "REFUSED" {
		t.Fatalf("expected second event response code REFUSED, got %q", second.ResponseCode)
	}
	if second.Upstream != rateLimitOverflowUpstream {
		t.Fatalf("expected second event upstream %q, got %q", rateLimitOverflowUpstream, second.Upstream)
	}

	time.Sleep(1100 * time.Millisecond)
	response = exchangeWithRetry(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR response after limiter refill, got rcode %d", response.Rcode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected proxy start error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after context cancellation")
	}
}

func TestProxyRateLimitsGlobally(t *testing.T) {
	engineAddr, stopEngine := startTestEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{
		ListenAddr:                   listenAddr,
		EngineAddr:                   engineAddr,
		EventChanSize:                8,
		GlobalRateLimitRPS:           1,
		GlobalRateLimitBurst:         1,
		PerSourceRateLimitRPS:        1000,
		PerSourceRateLimitBurst:      1000,
		PerSourceRateLimitStateTTL:   time.Minute,
		PerSourceRateLimitMaxSources: 100,
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	response := exchangeWithRetry(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR response, got rcode %d", response.Rcode)
	}

	response = exchangeWithRetry(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeRefused {
		t.Fatalf("expected REFUSED response due to global rate limit, got rcode %d", response.Rcode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected proxy start error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after context cancellation")
	}
}

func startTestEngine(t *testing.T) (string, func()) {
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
		message := new(dns.Msg)
		message.SetReply(r)
		if len(r.Question) > 0 && r.Question[0].Qtype == dns.TypeA {
			record, recordErr := dns.NewRR(fmt.Sprintf("%s 60 IN A 203.0.113.10", r.Question[0].Name))
			if recordErr == nil {
				message.Answer = append(message.Answer, record)
			}
		}
		_ = w.WriteMsg(message)
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

func startCountingEngine(t *testing.T) (string, *atomic.Int64, func()) {
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

	upstreamQueries := &atomic.Int64{}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		upstreamQueries.Add(1)

		message := new(dns.Msg)
		message.SetReply(r)
		if len(r.Question) > 0 && r.Question[0].Qtype == dns.TypeA {
			record, recordErr := dns.NewRR(fmt.Sprintf("%s 60 IN A 203.0.113.11", r.Question[0].Name))
			if recordErr == nil {
				message.Answer = append(message.Answer, record)
			}
		}
		_ = w.WriteMsg(message)
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

	return tcpListener.Addr().String(), upstreamQueries, stop
}

func freeLocalAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate free local address: %v", err)
	}
	defer listener.Close()

	return listener.Addr().String()
}

func TestProxyReturnsServfailWhenEngineUnreachable(t *testing.T) {
	// Point the proxy at an address where no DNS engine is running.
	// We allocate a free port and immediately close it so nothing listens.
	unreachableAddr := freeLocalAddr(t)

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{ListenAddr: listenAddr, EngineAddr: unreachableAddr, EventChanSize: 8})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	response := exchangeWithRetry(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeServerFailure {
		t.Fatalf("expected SERVFAIL (rcode %d), got rcode %d", dns.RcodeServerFailure, response.Rcode)
	}

	// Verify the event is emitted with SERVFAIL response code.
	select {
	case event := <-p.Events():
		if event.ResponseCode != "SERVFAIL" {
			t.Fatalf("expected event ResponseCode SERVFAIL, got %q", event.ResponseCode)
		}
		if event.Domain != "example.org" {
			t.Fatalf("expected event Domain example.org, got %q", event.Domain)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for query event")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected proxy start error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after context cancellation")
	}
}

func TestProxyReturnsServfailWhenEngineReturnsNilResponse(t *testing.T) {
	// Start an engine that reads packets but never responds.
	// This simulates a timeout in the proxy's internal DNS client, causing a nil response.
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create udp listener: %v", err)
	}

	silentAddr := conn.LocalAddr().String()

	go func() {
		buf := make([]byte, 4096)
		for {
			_, _, readErr := conn.ReadFrom(buf)
			if readErr != nil {
				return
			}
			// Intentionally do not respond.
		}
	}()
	defer conn.Close()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{ListenAddr: listenAddr, EngineAddr: silentAddr, EventChanSize: 8})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	// The proxy's internal client has a 2s timeout. Use a longer per-attempt
	// and total deadline so the caller waits long enough for the proxy to
	// receive the timeout and respond with SERVFAIL.
	response := exchangeWithLongTimeout(t, "udp", listenAddr, "timeout.example.org.")
	if response.Rcode != dns.RcodeServerFailure {
		t.Fatalf("expected SERVFAIL (rcode %d), got rcode %d", dns.RcodeServerFailure, response.Rcode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("unexpected proxy start error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not stop after context cancellation")
	}
}

// exchangeWithLongTimeout sends a DNS query with a generous timeout, suitable
// for tests where the proxy's internal exchange may take up to 2 seconds.
func exchangeWithLongTimeout(t *testing.T, network, addr, domain string) *dns.Msg {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		query := new(dns.Msg)
		query.SetQuestion(domain, dns.TypeA)

		client := &dns.Client{Net: network, Timeout: 5 * time.Second}
		response, _, err := client.Exchange(query, addr)
		if err == nil {
			return response
		}

		if time.Now().After(deadline) {
			t.Fatalf("dns query failed after retries: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func exchangeWithRetry(t *testing.T, network, addr, domain string) *dns.Msg {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		query := new(dns.Msg)
		query.SetQuestion(domain, dns.TypeA)

		client := &dns.Client{Net: network, Timeout: 200 * time.Millisecond}
		response, _, err := client.Exchange(query, addr)
		if err == nil {
			return response
		}

		if time.Now().After(deadline) {
			t.Fatalf("dns query failed after retries: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func awaitEvent(t *testing.T, events <-chan QueryEvent) QueryEvent {
	t.Helper()

	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for query event")
	}

	return QueryEvent{}
}
