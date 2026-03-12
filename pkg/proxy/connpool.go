package proxy

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/miekg/dns"
)

type dialEngineConnFn func(network, address string, timeout time.Duration) (*dns.Conn, error)

type engineConnPool struct {
	addr          string
	timeout       time.Duration
	maxPerNetwork int
	dial          dialEngineConnFn

	mu     sync.Mutex
	closed bool
	conns  map[string][]*dns.Conn
}

func newEngineConnPool(addr string, timeout time.Duration, maxPerNetwork int) *engineConnPool {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if maxPerNetwork <= 0 {
		maxPerNetwork = 1
	}

	return &engineConnPool{
		addr:          addr,
		timeout:       timeout,
		maxPerNetwork: maxPerNetwork,
		dial:          dns.DialTimeout,
		conns: map[string][]*dns.Conn{
			"udp": make([]*dns.Conn, 0, maxPerNetwork),
			"tcp": make([]*dns.Conn, 0, maxPerNetwork),
		},
	}
}

func (p *engineConnPool) Exchange(ctx context.Context, network string, request *dns.Msg) (*dns.Msg, error) {
	if p == nil {
		return nil, errors.New("engine connection pool is not initialized")
	}
	if request == nil {
		return nil, errors.New("dns request is required")
	}

	network = normalizeEngineNetwork(network)
	conn, err := p.borrow(network)
	if err != nil {
		return nil, err
	}

	client := &dns.Client{Net: network, Timeout: p.timeout}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(p.timeout))
	}

	response, _, err := client.ExchangeWithConnContext(ctx, request.Copy(), conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	p.release(network, conn)
	return response, nil
}

func (p *engineConnPool) Close() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true

	for network, pool := range p.conns {
		for _, conn := range pool {
			_ = conn.Close()
		}
		p.conns[network] = p.conns[network][:0]
	}
}

func (p *engineConnPool) borrow(network string) (*dns.Conn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("engine connection pool is closed")
	}
	pool := p.conns[network]
	if n := len(pool); n > 0 {
		conn := pool[n-1]
		p.conns[network] = pool[:n-1]
		p.mu.Unlock()
		return conn, nil
	}

	dialFn := p.dial
	addr := p.addr
	timeout := p.timeout
	p.mu.Unlock()

	return dialFn(network, addr, timeout)
}

func (p *engineConnPool) release(network string, conn *dns.Conn) {
	if conn == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = conn.Close()
		return
	}

	pool := p.conns[network]
	if len(pool) >= p.maxPerNetwork {
		_ = conn.Close()
		return
	}
	p.conns[network] = append(pool, conn)
}

func normalizeEngineNetwork(network string) string {
	if network == "tcp" {
		return "tcp"
	}

	return "udp"
}
