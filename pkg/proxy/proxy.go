package proxy

import (
	"context"
	"errors"
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
	ListenAddr                   string
	EngineAddr                   string
	QueryTimeout                 time.Duration
	EngineConnectionPoolSize     int
	EventChanSize                int
	CacheMaxEntries              int
	CacheDefaultTTL              time.Duration
	OnEventDrop                  func()
	GlobalRateLimitRPS           float64
	GlobalRateLimitBurst         int
	PerSourceRateLimitRPS        float64
	PerSourceRateLimitBurst      int
	PerSourceRateLimitStateTTL   time.Duration
	PerSourceRateLimitMaxSources int
	DomainFilterAllow            []string
	DomainFilterDeny             []string
	DomainFilterDenyRcode        int
}

const (
	defaultGlobalRateLimitRPS           = 2000
	defaultGlobalRateLimitBurst         = 4000
	defaultPerSourceRateLimitRPS        = 200
	defaultPerSourceRateLimitBurst      = 400
	defaultPerSourceRateLimitStateTTL   = 5 * time.Minute
	defaultPerSourceRateLimitMaxSources = 10000
	defaultEngineConnPoolSize           = 64
	defaultProxyCacheMaxEntries         = 10000
	defaultProxyCacheTTL                = 30 * time.Second

	rateLimitOverflowUpstream = "rate-limit"
	cacheHitUpstreamLabel     = "proxy-cache"
	cacheStaleUpstreamLabel   = "proxy-cache-stale"
)

// Proxy is a DNS proxy that intercepts queries and emits events.
type Proxy struct {
	config       ProxyConfig
	events       chan QueryEvent
	dropped      atomic.Int64
	limiter      *requestLimiter
	filter       *domainFilter
	filterRcode  int
	cache        *responseCache
	pool         *engineConnPool
	now          func() time.Time

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
	if config.QueryTimeout <= 0 {
		config.QueryTimeout = 2 * time.Second
	}
	if config.EventChanSize <= 0 {
		config.EventChanSize = 10000
	}
	if config.EngineConnectionPoolSize <= 0 {
		config.EngineConnectionPoolSize = defaultEngineConnPoolSize
	}
	if config.CacheMaxEntries <= 0 {
		config.CacheMaxEntries = defaultProxyCacheMaxEntries
	}
	if config.CacheDefaultTTL <= 0 {
		config.CacheDefaultTTL = defaultProxyCacheTTL
	}
	if config.GlobalRateLimitRPS <= 0 {
		config.GlobalRateLimitRPS = defaultGlobalRateLimitRPS
	}
	if config.GlobalRateLimitBurst <= 0 {
		config.GlobalRateLimitBurst = defaultGlobalRateLimitBurst
	}
	if config.PerSourceRateLimitRPS <= 0 {
		config.PerSourceRateLimitRPS = defaultPerSourceRateLimitRPS
	}
	if config.PerSourceRateLimitBurst <= 0 {
		config.PerSourceRateLimitBurst = defaultPerSourceRateLimitBurst
	}
	if config.PerSourceRateLimitStateTTL <= 0 {
		config.PerSourceRateLimitStateTTL = defaultPerSourceRateLimitStateTTL
	}
	if config.PerSourceRateLimitMaxSources <= 0 {
		config.PerSourceRateLimitMaxSources = defaultPerSourceRateLimitMaxSources
	}

	limiter := newRequestLimiter(config)
	filter := newDomainFilter(config.DomainFilterAllow, config.DomainFilterDeny)
	filterRcode := config.DomainFilterDenyRcode
	if filterRcode == 0 {
		filterRcode = dns.RcodeRefused
	}
	cache := newResponseCache(config.CacheMaxEntries, config.CacheDefaultTTL)
	pool := newEngineConnPool(config.EngineAddr, config.QueryTimeout, config.EngineConnectionPoolSize)

	return &Proxy{
		config:      config,
		events:      make(chan QueryEvent, config.EventChanSize),
		limiter:     limiter,
		filter:      filter,
		filterRcode: filterRcode,
		cache:       cache,
		pool:        pool,
		now:         time.Now,
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

// UpdateFilter replaces the domain filter rules at runtime.
// Pass nil/empty slices to disable filtering.
func (p *Proxy) UpdateFilter(allow, deny []string, rcode int) {
	f := newDomainFilter(allow, deny)
	if rcode == 0 {
		rcode = dns.RcodeRefused
	}

	p.mu.Lock()
	p.filter = f
	p.filterRcode = rcode
	p.mu.Unlock()
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
		if p.pool != nil {
			p.pool.Close()
		}

		p.inFlight.Wait()
		close(p.events)
	})
}

func (p *Proxy) handleQuery(w dns.ResponseWriter, request *dns.Msg) {
	start := p.now()
	srcIP := sourceIP(w.RemoteAddr())

	if p.limiter != nil && !p.limiter.Allow(srcIP, start) {
		response := new(dns.Msg)
		response.SetRcode(request, dns.RcodeRefused)

		_ = w.WriteMsg(response)
		p.emitQueryEvent(start, srcIP, request, response, rateLimitOverflowUpstream, false, false)

		return
	}

	p.mu.Lock()
	filter := p.filter
	filterRcode := p.filterRcode
	p.mu.Unlock()

	if filter != nil && !filter.Allowed(normalizeDomain(request)) {
		response := new(dns.Msg)
		response.SetRcode(request, filterRcode)

		_ = w.WriteMsg(response)
		p.emitDeniedEvent(start, srcIP, request, response)

		return
	}

	if p.cache != nil {
		cachedResponse, cached := p.cache.Get(request, start)
		if cached {
			_ = w.WriteMsg(cachedResponse)
			p.emitQueryEvent(start, srcIP, request, cachedResponse, cacheHitUpstreamLabel, true, true)
			return
		}
	}

	exchangeCtx, cancel := context.WithTimeout(context.Background(), p.config.QueryTimeout)
	defer cancel()

	response, err := p.pool.Exchange(exchangeCtx, networkForRemoteAddr(w.RemoteAddr()), request)
	if err != nil || response == nil {
		if p.cache != nil {
			staleResponse, stale := p.cache.GetStale(request, start)
			if stale {
				_ = w.WriteMsg(staleResponse)
				p.emitQueryEvent(start, srcIP, request, staleResponse, cacheStaleUpstreamLabel, true, true)
				return
			}
		}
		response = new(dns.Msg)
		response.SetRcode(request, dns.RcodeServerFailure)
	}

	if p.cache != nil {
		p.cache.Store(request, response, start)
	}

	_ = w.WriteMsg(response)
	p.emitQueryEvent(start, srcIP, request, response, p.config.EngineAddr, true, false)
}

func (p *Proxy) emitQueryEvent(
	start time.Time,
	srcIP string,
	request,
	response *dns.Msg,
	upstream string,
	cacheHitKnown,
	cacheHit bool,
) {
	latencyMs := float64(p.now().Sub(start).Microseconds()) / 1000.0

	event := QueryEvent{
		Timestamp:     start,
		SourceIP:      srcIP,
		Domain:        normalizeDomain(request),
		QueryType:     queryType(request),
		ResponseCode:  responseCode(response),
		Upstream:      upstream,
		LatencyMs:     latencyMs,
		CacheHitKnown: cacheHitKnown,
		CacheHit:      cacheHit,
	}
	p.publishEvent(event)
}

func (p *Proxy) emitDeniedEvent(
	start time.Time,
	srcIP string,
	request,
	response *dns.Msg,
) {
	latencyMs := float64(p.now().Sub(start).Microseconds()) / 1000.0

	event := QueryEvent{
		Timestamp:     start,
		SourceIP:      srcIP,
		Domain:        normalizeDomain(request),
		QueryType:     queryType(request),
		ResponseCode:  responseCode(response),
		Upstream:      deniedUpstreamLabel,
		LatencyMs:     latencyMs,
		CacheHitKnown: false,
		CacheHit:      false,
		Denied:        true,
	}
	p.publishEvent(event)
}

func (p *Proxy) publishEvent(event QueryEvent) {
	select {
	case p.events <- event:
	default:
		p.dropped.Add(1)
		if p.config.OnEventDrop != nil {
			p.config.OnEventDrop()
		}
	}
}

type requestLimiter struct {
	global *tokenBucket

	perSourceRate  float64
	perSourceBurst int
	stateTTL       time.Duration
	maxSources     int
	overflow       *tokenBucket

	mu           sync.Mutex
	perSource    map[string]*perSourceLimiter
	requestCount uint64
}

type perSourceLimiter struct {
	bucket   *tokenBucket
	lastSeen time.Time
}

func newRequestLimiter(config ProxyConfig) *requestLimiter {
	now := time.Now()
	globalLimiter := newTokenBucket(config.GlobalRateLimitRPS, config.GlobalRateLimitBurst, now)
	perSourceOverflow := newTokenBucket(config.PerSourceRateLimitRPS, config.PerSourceRateLimitBurst, now)
	if globalLimiter == nil && perSourceOverflow == nil {
		return nil
	}

	r := &requestLimiter{global: globalLimiter}
	if perSourceOverflow != nil {
		r.perSourceRate = config.PerSourceRateLimitRPS
		r.perSourceBurst = config.PerSourceRateLimitBurst
		r.stateTTL = config.PerSourceRateLimitStateTTL
		r.maxSources = config.PerSourceRateLimitMaxSources
		r.overflow = perSourceOverflow
		r.perSource = make(map[string]*perSourceLimiter)
	}

	return r
}

func (r *requestLimiter) Allow(source string, now time.Time) bool {
	if r == nil {
		return true
	}
	if r.global != nil && !r.global.Allow(now) {
		return false
	}
	if r.perSource == nil {
		return true
	}

	bucket := r.lookupPerSourceBucket(source, now)
	return bucket.Allow(now)
}

func (r *requestLimiter) lookupPerSourceBucket(source string, now time.Time) *tokenBucket {
	key := source
	if key == "" {
		key = "unknown"
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.requestCount++
	if r.requestCount%256 == 0 {
		r.cleanupExpiredLocked(now)
	}

	if existing, ok := r.perSource[key]; ok {
		existing.lastSeen = now
		return existing.bucket
	}

	if len(r.perSource) >= r.maxSources {
		r.cleanupExpiredLocked(now)
		if len(r.perSource) >= r.maxSources {
			return r.overflow
		}
	}

	bucket := newTokenBucket(r.perSourceRate, r.perSourceBurst, now)
	if bucket == nil {
		return r.overflow
	}

	r.perSource[key] = &perSourceLimiter{bucket: bucket, lastSeen: now}
	return bucket
}

func (r *requestLimiter) cleanupExpiredLocked(now time.Time) {
	if r.stateTTL <= 0 {
		return
	}

	for key, entry := range r.perSource {
		if now.Sub(entry.lastSeen) > r.stateTTL {
			delete(r.perSource, key)
		}
	}
}

type tokenBucket struct {
	rate float64

	mu         sync.Mutex
	burst      float64
	tokens     float64
	lastRefill time.Time
}

func newTokenBucket(rateLimitRPS float64, burst int, now time.Time) *tokenBucket {
	if rateLimitRPS <= 0 || burst <= 0 {
		return nil
	}

	burstFloat := float64(burst)
	return &tokenBucket{
		rate:       rateLimitRPS,
		burst:      burstFloat,
		tokens:     burstFloat,
		lastRefill: now,
	}
}

func (b *tokenBucket) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if now.Before(b.lastRefill) {
		b.lastRefill = now
	}

	elapsedSeconds := now.Sub(b.lastRefill).Seconds()
	if elapsedSeconds > 0 {
		b.tokens += elapsedSeconds * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.lastRefill = now
	}

	if b.tokens < 1 {
		return false
	}

	b.tokens--
	return true
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
	if errors.Is(err, net.ErrClosed) {
		return true
	}

	var opErr *net.OpError
	return errors.As(err, &opErr) && errors.Is(opErr.Err, net.ErrClosed)
}
