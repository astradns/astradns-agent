package proxy

import "testing"

func TestDomainFilterNilAllowsAll(t *testing.T) {
	f := newDomainFilter(nil, nil)
	if f != nil {
		t.Fatal("expected nil filter when no patterns provided")
	}

	// nil filter should allow everything
	var nilFilter *domainFilter
	if !nilFilter.Allowed("anything.example.com") {
		t.Fatal("nil filter should allow all domains")
	}
}

func TestDomainFilterAllowList(t *testing.T) {
	f := newDomainFilter([]string{"*.stripe.com", "api.sendgrid.com"}, nil)

	tests := []struct {
		domain  string
		allowed bool
	}{
		{"payments.stripe.com", true},
		{"api.stripe.com", true},
		{"stripe.com", true},
		{"api.sendgrid.com", true},
		{"evil.example.com", false},
		{"google.com", false},
		{"sendgrid.com", false},
	}

	for _, tt := range tests {
		if got := f.Allowed(tt.domain); got != tt.allowed {
			t.Errorf("Allowed(%q) = %v, want %v", tt.domain, got, tt.allowed)
		}
	}
}

func TestDomainFilterDenyList(t *testing.T) {
	f := newDomainFilter(nil, []string{"*.malware.example", "bad.actor.com"})

	tests := []struct {
		domain  string
		allowed bool
	}{
		{"google.com", true},
		{"safe.example.com", true},
		{"c2.malware.example", false},
		{"malware.example", false},
		{"bad.actor.com", false},
	}

	for _, tt := range tests {
		if got := f.Allowed(tt.domain); got != tt.allowed {
			t.Errorf("Allowed(%q) = %v, want %v", tt.domain, got, tt.allowed)
		}
	}
}

func TestDomainFilterAllowAndDenyCombined(t *testing.T) {
	// Allow only amazonaws.com, but deny a specific subdomain
	f := newDomainFilter(
		[]string{"*.amazonaws.com"},
		[]string{"*.s3.amazonaws.com"},
	)

	tests := []struct {
		domain  string
		allowed bool
	}{
		{"ec2.amazonaws.com", true},
		{"rds.amazonaws.com", true},
		{"mybucket.s3.amazonaws.com", false}, // denied by deny rule
		{"google.com", false},                // not in allow list
	}

	for _, tt := range tests {
		if got := f.Allowed(tt.domain); got != tt.allowed {
			t.Errorf("Allowed(%q) = %v, want %v", tt.domain, got, tt.allowed)
		}
	}
}

func TestDomainFilterWildcardStar(t *testing.T) {
	// deny: ["*"] blocks everything
	f := newDomainFilter([]string{"api.internal.com"}, []string{"*"})

	// Even though api.internal.com is in allow, deny "*" catches it
	if f.Allowed("api.internal.com") {
		t.Error("expected api.internal.com to be denied by wildcard deny")
	}
	if f.Allowed("anything.com") {
		t.Error("expected anything.com to be denied by wildcard deny")
	}
}

func TestDomainFilterCaseInsensitive(t *testing.T) {
	f := newDomainFilter([]string{"*.Example.COM"}, nil)

	if !f.Allowed("api.example.com") {
		t.Error("filter should be case-insensitive")
	}
	if !f.Allowed("API.EXAMPLE.COM") {
		t.Error("filter should be case-insensitive for input domain")
	}
}

func TestDomainFilterTrailingDot(t *testing.T) {
	f := newDomainFilter([]string{"example.com."}, nil)

	if !f.Allowed("example.com") {
		t.Error("filter should handle trailing dot in pattern")
	}
	if !f.Allowed("example.com.") {
		t.Error("filter should handle trailing dot in domain")
	}
}

func TestDomainFilterEmptyDomain(t *testing.T) {
	f := newDomainFilter([]string{"example.com"}, nil)

	if !f.Allowed("") {
		t.Error("empty domain should be allowed (root zone queries)")
	}
	if !f.Allowed(".") {
		t.Error("root dot should be allowed")
	}
}

func TestDomainFilterEmptyPatterns(t *testing.T) {
	f := newDomainFilter([]string{""}, []string{" "})
	if f != nil {
		t.Fatal("expected nil filter when all patterns are empty/whitespace")
	}
}
