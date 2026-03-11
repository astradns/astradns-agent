package proxy

import "time"

// QueryEvent represents a DNS query/response observed by the proxy.
type QueryEvent struct {
	Timestamp     time.Time
	SourceIP      string
	Domain        string
	QueryType     string
	ResponseCode  string
	Upstream      string
	LatencyMs     float64
	CacheHitKnown bool
	CacheHit      bool
}
