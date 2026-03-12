package proxy

import "strings"

const deniedUpstreamLabel = "domain-filter"

// domainFilter evaluates domain names against allow/deny pattern lists.
// When allow is non-empty, only matching domains pass. Deny is checked after
// allow — a domain matching both is denied.
type domainFilter struct {
	allow []domainPattern
	deny  []domainPattern
}

type domainPattern struct {
	wildcard bool   // true for patterns like "*.example.com"
	suffix   string // ".example.com" for wildcards, "example.com" for exact
}

func newDomainFilter(allow, deny []string) *domainFilter {
	allowPatterns := parsePatterns(allow)
	denyPatterns := parsePatterns(deny)
	if len(allowPatterns) == 0 && len(denyPatterns) == 0 {
		return nil
	}

	return &domainFilter{
		allow: allowPatterns,
		deny:  denyPatterns,
	}
}

// Allowed returns true if the domain passes the filter.
func (f *domainFilter) Allowed(domain string) bool {
	if f == nil {
		return true
	}

	normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if normalized == "" {
		return true
	}

	if len(f.allow) > 0 && !matchesAny(normalized, f.allow) {
		return false
	}

	if len(f.deny) > 0 && matchesAny(normalized, f.deny) {
		return false
	}

	return true
}

func parsePatterns(raw []string) []domainPattern {
	patterns := make([]domainPattern, 0, len(raw))
	for _, p := range raw {
		p = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(p), "."))
		if p == "" {
			continue
		}

		if p == "*" {
			patterns = append(patterns, domainPattern{wildcard: true, suffix: ""})
			continue
		}

		if strings.HasPrefix(p, "*.") {
			patterns = append(patterns, domainPattern{
				wildcard: true,
				suffix:   p[1:], // keep the dot: ".example.com"
			})
			continue
		}

		patterns = append(patterns, domainPattern{wildcard: false, suffix: p})
	}

	return patterns
}

func matchesAny(domain string, patterns []domainPattern) bool {
	for _, p := range patterns {
		if p.wildcard {
			if p.suffix == "" {
				return true // "*" matches everything
			}
			if strings.HasSuffix(domain, p.suffix) {
				return true
			}
			// "*.example.com" should also match "example.com" itself
			if domain == p.suffix[1:] {
				return true
			}
			continue
		}
		if domain == p.suffix {
			return true
		}
	}

	return false
}
