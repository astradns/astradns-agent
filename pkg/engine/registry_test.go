package engine_test

import (
	"testing"

	_ "github.com/astradns/astradns-agent/pkg/engine/coredns"
	_ "github.com/astradns/astradns-agent/pkg/engine/powerdns"
	_ "github.com/astradns/astradns-agent/pkg/engine/unbound"
	"github.com/astradns/astradns-types/engine"
)

func TestAllEnginesRegistered(t *testing.T) {
	available := engine.AvailableEngines()
	if len(available) != 3 {
		t.Fatalf("expected 3 engines, got %d: %v", len(available), available)
	}

	for _, et := range []engine.EngineType{engine.EngineUnbound, engine.EngineCoreDNS, engine.EnginePowerDNS} {
		e, err := engine.New(et, t.TempDir())
		if err != nil {
			t.Fatalf("failed to create %s engine: %v", et, err)
		}
		if e.Name() != et {
			t.Fatalf("expected Name() = %s, got %s", et, e.Name())
		}
	}
}
