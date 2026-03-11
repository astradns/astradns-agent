package proxy

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// ProxyConfig holds configuration for the DNS proxy.
type ProxyConfig struct {
	ListenAddr    string
	EngineAddr    string
	EventChanSize int
}

// Proxy is a DNS proxy that intercepts queries and emits events.
type Proxy struct {
	config  ProxyConfig
	events  chan QueryEvent
	dropped atomic.Int64

	mu       sync.Mutex
	udp      *dns.Server
	tcp      *dns.Server
	stopped  sync.Once
	inFlight sync.WaitGroup
}

// New creates a new DNS proxy instance.
func New(config ProxyConfig) *Proxy {
	if config.ListenAddr == "" {
		config.ListenAddr = "0.0.0.0:5353"
	}
	if config.EngineAddr == "" {
		config.EngineAddr = "127.0.0.1:5354"
	}
	if config.EventChanSize <= 0 {
		config.EventChanSize = 10000
	}

	return &Proxy{
		config: config,
		events: make(chan QueryEvent, config.EventChanSize),
	}
}

// Events returns the read-only query event channel.
func (p *Proxy) Events() <-chan QueryEvent {
	return p.events
}

// DroppedEvents returns the number of dropped query events.
func (p *Proxy) DroppedEvents() int64 {
	return p.dropped.Load()
}

// Start starts the proxy listeners and blocks until the context is canceled.
func (p *Proxy) Start(ctx context.Context) error {
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		p.inFlight.Add(1)
		defer p.inFlight.Done()
		p.handleQuery(w, r)
	})

	p.mu.Lock()
	if p.udp != nil || p.tcp != nil {
		p.mu.Unlock()
		return fmt.Errorf("proxy already started")
	}
	p.udp = &dns.Server{Addr: p.config.ListenAddr, Net: "udp", Handler: handler}
	p.tcp = &dns.Server{Addr: p.config.ListenAddr, Net: "tcp", Handler: handler}
	udpServer := p.udp
	tcpServer := p.tcp
	p.mu.Unlock()

	errCh := make(chan error, 2)
	go func() {
		if err := udpServer.ListenAndServe(); err != nil && !isShutdownErr(err) {
			errCh <- fmt.Errorf("udp listener failed: %w", err)
		}
	}()
	go func() {
		if err := tcpServer.ListenAndServe(); err != nil && !isShutdownErr(err) {
			errCh <- fmt.Errorf("tcp listener failed: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		p.Stop()
		return nil
	case err := <-errCh:
		p.Stop()
		return err
	}
}

// Stop gracefully shuts down the proxy.
func (p *Proxy) Stop() {
	p.stopped.Do(func() {
		p.mu.Lock()
		udpServer := p.udp
		tcpServer := p.tcp
		p.udp = nil
		p.tcp = nil
		p.mu.Unlock()

		if udpServer != nil {
			_ = udpServer.Shutdown()
		}
		if tcpServer != nil {
			_ = tcpServer.Shutdown()
		}

		p.inFlight.Wait()
		close(p.events)
	})
}

func (p *Proxy) handleQuery(w dns.ResponseWriter, request *dns.Msg) {
	start := time.Now()
	client := &dns.Client{Net: networkForRemoteAddr(w.RemoteAddr()), Timeout: 2 * time.Second}

	response, _, err := client.Exchange(request.Copy(), p.config.EngineAddr)
	if err != nil || response == nil {
		response = new(dns.Msg)
		response.SetRcode(request, dns.RcodeServerFailure)
	}

	_ = w.WriteMsg(response)

	latencyMs := float64(time.Since(start).Microseconds()) / 1000.0

	event := QueryEvent{
		Timestamp:     start,
		SourceIP:      sourceIP(w.RemoteAddr()),
		Domain:        normalizeDomain(request),
		QueryType:     queryType(request),
		ResponseCode:  responseCode(response),
		Upstream:      p.config.EngineAddr,
		LatencyMs:     latencyMs,
		CacheHitKnown: false,
		CacheHit:      false,
	}

	select {
	case p.events <- event:
	default:
		p.dropped.Add(1)
	}
}

func queryType(request *dns.Msg) string {
	if request == nil || len(request.Question) == 0 {
		return "UNKNOWN"
	}
	qType := request.Question[0].Qtype
	if value, ok := dns.TypeToString[qType]; ok {
		return value
	}
	return fmt.Sprintf("TYPE%d", qType)
}

func responseCode(response *dns.Msg) string {
	if response == nil {
		return "SERVFAIL"
	}
	if value, ok := dns.RcodeToString[response.Rcode]; ok {
		return value
	}
	return fmt.Sprintf("RCODE%d", response.Rcode)
}

func normalizeDomain(request *dns.Msg) string {
	if request == nil || len(request.Question) == 0 {
		return ""
	}
	return strings.TrimSuffix(request.Question[0].Name, ".")
}

func sourceIP(remote net.Addr) string {
	if remote == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		return remote.String()
	}
	return host
}

func networkForRemoteAddr(remote net.Addr) string {
	switch remote.(type) {
	case *net.TCPAddr:
		return "tcp"
	default:
		return "udp"
	}
}

func isShutdownErr(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "closed") {
		return true
	}
	if strings.Contains(message, "shutdown") {
		return true
	}
	return false
}
