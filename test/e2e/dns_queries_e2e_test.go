//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/astradns/astradns-agent/pkg/proxy"
	"github.com/miekg/dns"
)

func TestDNSQueriesOverUDPAndTCP(t *testing.T) {
	engineAddr, stopEngine := startE2EEngine(t)
	defer stopEngine()

	listenAddr := reserveAddr(t)
	proxyInstance := proxy.New(proxy.ProxyConfig{
		ListenAddr:    listenAddr,
		EngineAddr:    engineAddr,
		EventChanSize: 32,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxyErr := make(chan error, 1)
	go func() {
		proxyErr <- proxyInstance.Start(ctx)
	}()

	udpResponse := queryDNS(t, "udp", listenAddr, "example.org.")
	if udpResponse.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected UDP NOERROR response, got rcode %d", udpResponse.Rcode)
	}

	tcpResponse := queryDNS(t, "tcp", listenAddr, "example.org.")
	if tcpResponse.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected TCP NOERROR response, got rcode %d", tcpResponse.Rcode)
	}

	if len(udpResponse.Answer) == 0 || len(tcpResponse.Answer) == 0 {
		t.Fatal("expected both UDP and TCP queries to return answers")
	}

	cancel()
	select {
	case err := <-proxyErr:
		if err != nil {
			t.Fatalf("proxy returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not stop after cancellation")
	}
}

func TestDNSReturnsServfailWhenUpstreamStops(t *testing.T) {
	engineAddr, stopEngine := startE2EEngine(t)

	listenAddr := reserveAddr(t)
	proxyInstance := proxy.New(proxy.ProxyConfig{
		ListenAddr:    listenAddr,
		EngineAddr:    engineAddr,
		EventChanSize: 32,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proxyErr := make(chan error, 1)
	go func() {
		proxyErr <- proxyInstance.Start(ctx)
	}()

	response := queryDNS(t, "udp", listenAddr, "example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected initial NOERROR response, got rcode %d", response.Rcode)
	}

	stopEngine()

	servfail := queryDNS(t, "udp", listenAddr, "after-stop.example.org.")
	if servfail.Rcode != dns.RcodeServerFailure {
		t.Fatalf("expected SERVFAIL after upstream stop, got rcode %d", servfail.Rcode)
	}

	cancel()
	select {
	case err := <-proxyErr:
		if err != nil {
			t.Fatalf("proxy returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not stop after cancellation")
	}
}

func startE2EEngine(t *testing.T) (string, func()) {
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
			record, recordErr := dns.NewRR(fmt.Sprintf("%s 30 IN A 203.0.113.55", r.Question[0].Name))
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

func reserveAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve local address: %v", err)
	}
	defer listener.Close()

	return listener.Addr().String()
}

func queryDNS(t *testing.T, network, addr, domain string) *dns.Msg {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for {
		query := new(dns.Msg)
		query.SetQuestion(domain, dns.TypeA)

		client := &dns.Client{Net: network, Timeout: 300 * time.Millisecond}
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
