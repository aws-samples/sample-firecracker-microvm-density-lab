// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package warmpool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/host-agent/internal/orchestrator"
)

// fakeLauncher stands in for the orchestrator so the pool can be tested without root,
// cgroups, or a Firecracker binary. It records launches and stops.
type fakeLauncher struct {
	density int

	mu       sync.Mutex
	launched int
	stopped  int
	counter  int64
}

func (f *fakeLauncher) Launch(_ context.Context, slot int) (*orchestrator.MicroVM, error) {
	n := atomic.AddInt64(&f.counter, 1)
	f.mu.Lock()
	f.launched++
	f.mu.Unlock()
	return &orchestrator.MicroVM{
		Index: int(n),
		ID:    fmt.Sprintf("vm%d", n),
		Node:  slot % 2, // spread across two NUMA nodes
	}, nil
}

func (f *fakeLauncher) Stop(_ *orchestrator.MicroVM) error {
	f.mu.Lock()
	f.stopped++
	f.mu.Unlock()
	return nil
}

func (f *fakeLauncher) PlannedDensity() int { return f.density }

func (f *fakeLauncher) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launched, f.stopped
}

func TestStartFillsToTarget(t *testing.T) {
	f := &fakeLauncher{density: 32}
	p := New(f, 4, nil, nil)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	if got := p.Size(); got != 4 {
		t.Fatalf("warm pool size = %d, want 4", got)
	}
	if launched, _ := f.counts(); launched != 4 {
		t.Fatalf("launched = %d, want 4", launched)
	}
}

func TestAcquireHandsOutWarmVM(t *testing.T) {
	f := &fakeLauncher{density: 32}
	p := New(f, 2, nil, nil)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	vm, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if vm == nil {
		t.Fatal("Acquire returned nil microVM")
	}
	if p.InUse() != 1 {
		t.Fatalf("in-use = %d, want 1", p.InUse())
	}

	// The pool should asynchronously refill back to target.
	if !waitFor(func() bool { return p.Size() == 2 }, time.Second) {
		t.Fatalf("pool did not refill to target; size = %d", p.Size())
	}
}

func TestAcquireOnDemandWhenEmpty(t *testing.T) {
	f := &fakeLauncher{density: 32}
	p := New(f, 0, nil, nil) // no warm pool
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	vm, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire on-demand: %v", err)
	}
	if vm == nil {
		t.Fatal("on-demand Acquire returned nil")
	}
	if p.InUse() != 1 {
		t.Fatalf("in-use = %d, want 1", p.InUse())
	}
}

func TestReleaseStopsAndRefills(t *testing.T) {
	f := &fakeLauncher{density: 32}
	p := New(f, 2, nil, nil)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	vm, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	p.Release(vm)

	if !waitFor(func() bool {
		_, stopped := f.counts()
		return stopped >= 1 && p.Size() == 2
	}, time.Second) {
		t.Fatalf("release did not stop+refill; size=%d", p.Size())
	}
}

func TestAcquireAfterCloseFails(t *testing.T) {
	f := &fakeLauncher{density: 32}
	p := New(f, 1, nil, nil)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.Close()

	if _, err := p.Acquire(context.Background()); err != ErrClosed {
		t.Fatalf("Acquire after close = %v, want ErrClosed", err)
	}
}

func TestCloseStopsAllVMs(t *testing.T) {
	f := &fakeLauncher{density: 32}
	p := New(f, 3, nil, nil)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	vm, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	_ = vm
	// Give any refill a moment to settle.
	waitFor(func() bool { return p.Size() == 3 }, time.Second)

	p.Close()
	launched, stopped := f.counts()
	if stopped != launched {
		t.Fatalf("stopped %d of %d launched; all should be stopped", stopped, launched)
	}
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}
