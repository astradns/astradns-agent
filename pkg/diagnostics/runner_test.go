package diagnostics

import (
	"errors"
	"net/http"
	"testing"
	time "time"
)

func TestClassifyOutcome(t *testing.T) {
	tests := []struct {
		name      string
		outcome   probeOutcome
		diagnosis Diagnosis
	}{
		{
			name: "dns failure",
			outcome: probeOutcome{
				dnsErr: errors.New("lookup timeout"),
			},
			diagnosis: DiagnosisDNSResolutionFailed,
		},
		{
			name: "egress failure",
			outcome: probeOutcome{
				resolvedIPs: []string{"1.1.1.1"},
				tcpErr:      errors.New("dial timeout"),
			},
			diagnosis: DiagnosisEgressBlockedOrNetwork,
		},
		{
			name: "tls failure",
			outcome: probeOutcome{
				resolvedIPs:   []string{"1.1.1.1"},
				tcpReachable:  true,
				tlsWithSNIErr: errors.New("tls handshake failed"),
			},
			diagnosis: DiagnosisTLSHandshakeFailed,
		},
		{
			name: "tls sni required",
			outcome: probeOutcome{
				resolvedIPs:      []string{"1.1.1.1"},
				tcpReachable:     true,
				tlsWithoutSNIErr: errors.New("x509: cannot validate certificate for 1.1.1.1"),
			},
			diagnosis: DiagnosisTLSSNIRequired,
		},
		{
			name: "provider block suspected",
			outcome: probeOutcome{
				resolvedIPs:    []string{"1.1.1.1"},
				tcpReachable:   true,
				httpStatusCode: http.StatusForbidden,
			},
			diagnosis: DiagnosisProviderBlockSuspected,
		},
		{
			name: "healthy",
			outcome: probeOutcome{
				resolvedIPs:  []string{"1.1.1.1"},
				tcpReachable: true,
			},
			diagnosis: DiagnosisHealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diagnosis, _, _, _ := classifyOutcome(tt.outcome)
			if diagnosis != tt.diagnosis {
				t.Fatalf("expected diagnosis %q, got %q", tt.diagnosis, diagnosis)
			}
		})
	}
}

func TestNewRunnerNormalizesTargets(t *testing.T) {
	runner := NewRunner(Config{
		Targets:      []string{" EXAMPLE.com ", "example.com", "bad target", ""},
		ResolverAddr: "",
		Interval:     0,
		Timeout:      0,
	}, nil)

	if runner.cfg.ResolverAddr != defaultResolverAddr {
		t.Fatalf("expected default resolver %q, got %q", defaultResolverAddr, runner.cfg.ResolverAddr)
	}
	if runner.cfg.Interval != defaultInterval {
		t.Fatalf("expected default interval %s, got %s", defaultInterval, runner.cfg.Interval)
	}
	if runner.cfg.Timeout != defaultTimeout {
		t.Fatalf("expected default timeout %s, got %s", defaultTimeout, runner.cfg.Timeout)
	}
	if len(runner.cfg.Targets) != 1 || runner.cfg.Targets[0] != "example.com" {
		t.Fatalf("unexpected normalized targets: %v", runner.cfg.Targets)
	}
}

func TestSnapshotReturnsSortedCopy(t *testing.T) {
	runner := NewRunner(Config{Targets: []string{"b.example", "a.example"}}, nil)
	now := time.Unix(10, 0)
	runner.now = func() time.Time { return now }

	runner.mu.Lock()
	runner.lastUpdated = now
	runner.results["b.example"] = Result{Target: "b.example", ResolvedIPs: []string{"2.2.2.2"}}
	runner.results["a.example"] = Result{Target: "a.example", ResolvedIPs: []string{"1.1.1.1"}}
	runner.mu.Unlock()

	snapshot := runner.Snapshot()
	if len(snapshot.Results) != 2 {
		t.Fatalf("expected 2 snapshot results, got %d", len(snapshot.Results))
	}
	if snapshot.Results[0].Target != "a.example" || snapshot.Results[1].Target != "b.example" {
		t.Fatalf("expected sorted targets, got %v and %v", snapshot.Results[0].Target, snapshot.Results[1].Target)
	}

	snapshot.Results[0].ResolvedIPs[0] = "9.9.9.9"
	runner.mu.RLock()
	original := runner.results["a.example"].ResolvedIPs[0]
	runner.mu.RUnlock()
	if original != "1.1.1.1" {
		t.Fatalf("expected snapshot mutation not to affect stored state, got %s", original)
	}
}
