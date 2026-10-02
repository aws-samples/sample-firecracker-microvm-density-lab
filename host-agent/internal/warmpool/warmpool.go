// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package warmpool maintains a pool of pre-booted microVMs and hands them out on dispatch.
//
// This is the fourth Host Agent duty from the architecture diagram: manage a warm pool for
// dispatch. The diagram distinguishes a Warm Pool of pre-booted Firecracker microVMs from
// On-Demand microVMs launched beyond the pool. This package implements both: Acquire hands
// out a warm microVM instantly when one is available, refills the pool asynchronously, and
// falls back to an on-demand launch when the pool is empty.
//
// A pre-booted warm pool is cheap because the fleet shares one read-only rootfs page-cache
// copy and one kernel image, so idle microVMs cost little beyond their pinned cores and
// guest memory. Handing one out removes cold-boot latency from the dispatch path.
package warmpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/host-agent/internal/metrics"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/host-agent/internal/orchestrator"
)

// Launcher is the subset of the orchestrator the pool needs. Defined as an interface so
// the pool can be tested against a fake and to keep the dependency direction explicit.
type Launcher interface {
	Launch(ctx context.Context, planIndex int) (*orchestrator.MicroVM, error)
	Stop(vm *orchestrator.MicroVM) error
	PlannedDensity() int
}

// Pool is a thread-safe warm pool of pre-booted microVMs.
type Pool struct {
	launcher Launcher
	target   int
	log      *slog.Logger
	metrics  *metrics.Metrics

	mu       sync.Mutex
	idle     []*orchestrator.MicroVM // pre-booted, not yet handed out
	inUse    map[string]*orchestrator.MicroVM
	onDemand map[string]struct{} // subset of inUse launched beyond the warm pool
	nextSlot int                 // round-robins across the plan's placement slots
	closed   bool

	// refillCh signals the background refiller to top up the pool.
	refillCh chan struct{}
	wg       sync.WaitGroup
}

// ErrClosed is returned by Acquire after the pool has been closed.
var ErrClosed = errors.New("warm pool is closed")

// New creates a warm pool with the given target size. Call Start to boot the initial pool
// and begin the background refiller.
func New(l Launcher, target int, log *slog.Logger, m *metrics.Metrics) *Pool {
	if log == nil {
		log = slog.Default()
	}
	if target < 0 {
		target = 0
	}
	return &Pool{
		launcher: l,
		target:   target,
		log:      log,
		metrics:  m,
		inUse:    make(map[string]*orchestrator.MicroVM),
		onDemand: make(map[string]struct{}),
		refillCh: make(chan struct{}, 1),
	}
}

// Start boots the initial warm pool up to the target and launches the background
// refiller. It blocks until the initial fill completes (or the context is cancelled).
func (p *Pool) Start(ctx context.Context) error {
	if p.metrics != nil {
		p.metrics.WarmPoolTarget.Set(float64(p.target))
	}
	p.wg.Add(1)
	go p.refillLoop(ctx)

	// Initial synchronous fill so the pool is warm before the agent reports ready.
	for i := 0; i < p.target; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		vm, err := p.launchOne(ctx)
		if err != nil {
			// A partial pool is still useful; log and continue so one bad slot does not
			// prevent the rest from booting.
			p.log.Warn("initial warm pool launch failed", "err", err)
			continue
		}
		p.mu.Lock()
		p.idle = append(p.idle, vm)
		p.mu.Unlock()
	}
	p.updateGauges()
	p.log.Info("warm pool started", "target", p.target, "idle", p.Size())
	return nil
}

// Acquire returns a ready microVM for dispatch. It hands out a warm one when available and
// otherwise launches on demand, subject to the plan's density ceiling. The returned
// microVM is tracked as in-use until Release is called.
func (p *Pool) Acquire(ctx context.Context) (*orchestrator.MicroVM, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	if n := len(p.idle); n > 0 {
		vm := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.inUse[vm.ID] = vm
		p.mu.Unlock()

		p.signalRefill()
		p.recordAcquire("warm")
		p.updateGauges()
		return vm, nil
	}
	p.mu.Unlock()

	// Pool empty: launch on demand beyond the warm pool, as the diagram's On-Demand path.
	vm, err := p.launchOne(ctx)
	if err != nil {
		p.recordAcquire("error")
		return nil, fmt.Errorf("on-demand launch: %w", err)
	}
	p.mu.Lock()
	p.inUse[vm.ID] = vm
	p.onDemand[vm.ID] = struct{}{}
	p.mu.Unlock()

	p.recordAcquire("ondemand")
	p.updateGauges()
	return vm, nil
}

// Release returns an in-use microVM to the caller's disposal. Because a used microVM has
// mutated guest state, it is torn down rather than returned to the idle pool, and the
// refiller boots a fresh one to keep the pool at target. This preserves the clean-slate
// guarantee each dispatch expects.
func (p *Pool) Release(vm *orchestrator.MicroVM) {
	if vm == nil {
		return
	}
	p.mu.Lock()
	delete(p.inUse, vm.ID)
	delete(p.onDemand, vm.ID)
	p.mu.Unlock()

	if err := p.launcher.Stop(vm); err != nil {
		p.log.Warn("stop released microVM", "id", vm.ID, "err", err)
	}
	p.signalRefill()
	p.updateGauges()
}

// Size returns the number of idle (warm, ready) microVMs.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// InUse returns the number of microVMs currently handed out.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inUse)
}

// Close stops the refiller and tears down every microVM the pool owns (idle and in-use).
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	inUse := make([]*orchestrator.MicroVM, 0, len(p.inUse))
	for _, vm := range p.inUse {
		inUse = append(inUse, vm)
	}
	p.inUse = map[string]*orchestrator.MicroVM{}
	p.onDemand = map[string]struct{}{}
	p.mu.Unlock()

	// Signal and wait for the refiller to exit.
	close(p.refillCh)
	p.wg.Wait()

	for _, vm := range append(idle, inUse...) {
		if err := p.launcher.Stop(vm); err != nil {
			p.log.Warn("stop microVM during close", "id", vm.ID, "err", err)
		}
	}
	p.updateGauges()
	p.log.Info("warm pool closed")
}

// refillLoop tops the pool back up to target after Acquire/Release drain it. It runs in
// the background so refills never block the dispatch path.
func (p *Pool) refillLoop(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-p.refillCh:
			if !ok {
				return
			}
			p.refill(ctx)
		}
	}
}

// refill launches microVMs until the idle pool reaches target, one at a time so a burst of
// signals does not oversubscribe launches.
func (p *Pool) refill(ctx context.Context) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		need := p.target - len(p.idle)
		p.mu.Unlock()
		if need <= 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		vm, err := p.launchOne(ctx)
		if err != nil {
			p.log.Warn("warm pool refill launch failed", "err", err)
			return // back off; the next Acquire/Release will re-signal
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = p.launcher.Stop(vm)
			return
		}
		p.idle = append(p.idle, vm)
		p.mu.Unlock()
		if p.metrics != nil {
			p.metrics.RefillsTotal.Inc()
		}
		p.updateGauges()
	}
}

// launchOne launches a single microVM at the next placement slot, round-robining across
// the plan so refills spread evenly across NUMA nodes the same way the plan does.
func (p *Pool) launchOne(ctx context.Context) (*orchestrator.MicroVM, error) {
	density := p.launcher.PlannedDensity()
	if density <= 0 {
		return nil, errors.New("no placement slots available")
	}
	p.mu.Lock()
	slot := p.nextSlot % density
	p.nextSlot++
	p.mu.Unlock()

	start := time.Now()
	vm, err := p.launcher.Launch(ctx, slot)
	if err != nil {
		if p.metrics != nil {
			p.metrics.LaunchFailures.Inc()
		}
		return nil, err
	}
	if p.metrics != nil {
		p.metrics.LaunchLatency.Observe(time.Since(start).Seconds())
		p.metrics.LaunchesTotal.WithLabelValues("warm").Inc()
	}
	return vm, nil
}

func (p *Pool) signalRefill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return // channel is closed; sending would panic
	}
	select {
	case p.refillCh <- struct{}{}:
	default: // a refill is already queued; one signal is enough
	}
}

func (p *Pool) recordAcquire(outcome string) {
	if p.metrics != nil {
		p.metrics.AcquireTotal.WithLabelValues(outcome).Inc()
	}
}

// updateGauges syncs the size/in-use/per-node gauges with current state.
func (p *Pool) updateGauges() {
	if p.metrics == nil {
		return
	}
	p.mu.Lock()
	idle := len(p.idle)
	inUse := len(p.inUse)
	onDemand := len(p.onDemand)
	byNode := map[int]int{}
	for _, vm := range p.idle {
		byNode[vm.Node]++
	}
	for _, vm := range p.inUse {
		byNode[vm.Node]++
	}
	p.mu.Unlock()

	p.metrics.WarmPoolSize.Set(float64(idle))
	p.metrics.InUse.Set(float64(inUse))
	p.metrics.OnDemand.Set(float64(onDemand))
	p.metrics.SetVMsByNode(byNode)
}
