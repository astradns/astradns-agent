package proxy

import (
	"context"
	"fmt"
	"net"
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

func TestProxyDropsEventsWhenChannelIsFull(t *testing.T) {
	engineAddr, stopEngine := startTestEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{ListenAddr: listenAddr, EngineAddr: engineAddr, EventChanSize: 1})

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

func freeLocalAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate free local address: %v", err)
	}
	defer listener.Close()

	return listener.Addr().String()
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
