package main

import (
	"sync/atomic"
	"testing"

	"github.com/astradns/astradns-agent/pkg/proxy"
)

func TestFanOutReportsDroppedEvents(t *testing.T) {
	in := make(chan proxy.QueryEvent, 1)
	outBuffered := make(chan proxy.QueryEvent, 1)
	outDropped := make(chan proxy.QueryEvent)

	in <- proxy.QueryEvent{QueryType: "A"}
	close(in)

	var dropped atomic.Int64
	fanOut(in, func() {
		dropped.Add(1)
	}, outBuffered, outDropped)

	if dropped.Load() != 1 {
		t.Fatalf("expected one dropped event, got %d", dropped.Load())
	}

	if _, ok := <-outBuffered; !ok {
		t.Fatal("expected buffered output to receive event before close")
	}

	if _, ok := <-outBuffered; ok {
		t.Fatal("expected buffered output channel to be closed")
	}

	if _, ok := <-outDropped; ok {
		t.Fatal("expected dropped output channel to be closed")
	}
}
