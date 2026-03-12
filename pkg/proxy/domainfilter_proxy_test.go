package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestProxyDeniesQueriesMatchingDenyList(t *testing.T) {
	engineAddr, stopEngine := startTestEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{
		ListenAddr:        listenAddr,
		EngineAddr:        engineAddr,
		EventChanSize:     8,
		DomainFilterDeny:  []string{"*.malware.example"},
		DomainFilterDenyRcode: dns.RcodeRefused,
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	// Allowed domain should succeed
	response := exchangeWithRetry(t, "udp", listenAddr, "safe.example.org.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR for allowed domain, got rcode %d", response.Rcode)
	}

	// Denied domain should get REFUSED
	response = exchangeWithRetry(t, "udp", listenAddr, "c2.malware.example.")
	if response.Rcode != dns.RcodeRefused {
		t.Fatalf("expected REFUSED for denied domain, got rcode %d", response.Rcode)
	}

	// Verify events
	allowedEvent := awaitEvent(t, p.Events())
	if allowedEvent.Domain != "safe.example.org" {
		t.Fatalf("expected allowed event domain safe.example.org, got %q", allowedEvent.Domain)
	}
	if allowedEvent.Denied {
		t.Fatal("expected allowed event to not be denied")
	}

	deniedEvent := awaitEvent(t, p.Events())
	if deniedEvent.Domain != "c2.malware.example" {
		t.Fatalf("expected denied event domain c2.malware.example, got %q", deniedEvent.Domain)
	}
	if !deniedEvent.Denied {
		t.Fatal("expected denied event to be marked as denied")
	}
	if deniedEvent.Upstream != deniedUpstreamLabel {
		t.Fatalf("expected denied event upstream %q, got %q", deniedUpstreamLabel, deniedEvent.Upstream)
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

func TestProxyAllowListBlocksUnlistedDomains(t *testing.T) {
	engineAddr, stopEngine := startTestEngine(t)
	defer stopEngine()

	listenAddr := freeLocalAddr(t)
	p := New(ProxyConfig{
		ListenAddr:            listenAddr,
		EngineAddr:            engineAddr,
		EventChanSize:         8,
		DomainFilterAllow:     []string{"*.approved.com"},
		DomainFilterDenyRcode: dns.RcodeNameError, // NXDOMAIN
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start(ctx)
	}()

	// Allowed domain
	response := exchangeWithRetry(t, "udp", listenAddr, "api.approved.com.")
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected NOERROR for allowed domain, got rcode %d", response.Rcode)
	}

	// Unlisted domain should get NXDOMAIN
	response = exchangeWithRetry(t, "udp", listenAddr, "evil.other.com.")
	if response.Rcode != dns.RcodeNameError {
		t.Fatalf("expected NXDOMAIN for unlisted domain, got rcode %d", response.Rcode)
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
