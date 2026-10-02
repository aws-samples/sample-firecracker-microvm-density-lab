// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Command host-agent is the per-node Host Agent binary from the architecture diagram.
//
// It runs one copy per EC2 metal host: directly as a systemd service on the lab reference
// host (see systemd/host-agent.service), or as a privileged DaemonSet in a production EKS
// deployment (guidance in the repo README). On startup it discovers the host's CPU
// topology, builds a placement plan for a chosen configuration and density, establishes the
// parent cgroup hierarchy, boots a warm pool of pinned Firecracker microVMs, and serves a
// Prometheus endpoint. On SIGINT/SIGTERM it drains the pool and tears down the cgroup
// hierarchy so the node is left clean.
//
// The four duties the diagram assigns the Host Agent map to internal packages:
//   - discover CPU topology            -> internal/hostinfo
//   - create per-microVM cgroups and   -> internal/orchestrator
//     orchestrate jailer processes
//   - manage a warm pool for dispatch  -> internal/warmpool
//   - (observability for all of it)    -> internal/metrics
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/host-agent/internal/hostinfo"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/host-agent/internal/metrics"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/host-agent/internal/orchestrator"
	"github.com/aws-samples/sample-firecracker-microvm-density-lab/host-agent/internal/warmpool"

	"github.com/aws-samples/sample-firecracker-microvm-density-lab/lab/placement"
)

func main() {
	var (
		configName   = flag.String("config", "D", "placement configuration A-F (see lab/placement)")
		density      = flag.Int("density", 96, "number of microVM placement slots to plan")
		warmPoolSize = flag.Int("warm-pool-size", 16, "number of pre-booted microVMs to keep ready")
		// Loopback by default: the endpoint carries no secrets, but a node that runs
		// untrusted workloads should not expose unauthenticated listeners it does not
		// need. Production scrapers (e.g. a Prometheus DaemonSet pattern) override this
		// deliberately with -metrics-addr=:9090.
		metricsAddr    = flag.String("metrics-addr", "127.0.0.1:9090", "address for the Prometheus metrics endpoint")
		kernelPath     = flag.String("kernel", "/opt/firecracker/vmlinux", "host path to the guest kernel (6.18)")
		rootfsPath     = flag.String("rootfs", "/opt/firecracker/rootfs.ext4", "host path to the read-only rootfs")
		instanceType   = flag.String("instance-type", "", "EC2 instance type, e.g. m8i.metal-48xl or m8a.metal-48xl (informational)")
		jailerBin      = flag.String("jailer", "/usr/local/bin/jailer", "path to the jailer binary")
		firecrackerBin = flag.String("firecracker", "/usr/local/bin/firecracker", "path to the firecracker binary")
		memMiB         = flag.Int("mem-mib", 2048, "guest memory per microVM in MiB")
		vcpus          = flag.Int("vcpus", 1, "guest vCPU count per microVM")
		cgroupName     = flag.String("cgroup-name", "hostagent", "parent cgroup directory name under /sys/fs/cgroup")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := placement.Config(strings.ToUpper(strings.TrimSpace(*configName)))
	if !validConfig(cfg) {
		log.Error("invalid placement config", "config", *configName, "valid", placement.AllConfigs)
		os.Exit(2)
	}

	log.Info("host-agent starting",
		"config", string(cfg),
		"density", *density,
		"warm_pool_size", *warmPoolSize,
		"instance_type", *instanceType,
		"firecracker_version", orchestrator.FirecrackerVersion,
		"guest_kernel_version", orchestrator.GuestKernelVersion,
	)

	// Duty 1: discover CPU topology.
	host, err := hostinfo.Discover()
	if err != nil {
		log.Error("topology discovery failed", "err", err)
		os.Exit(1)
	}
	host.Log(log)

	// A config that requires SMT cannot run on an AMD metal host (one thread per core).
	// Fail early with a clear message rather than deep in placement.Build.
	if cfg.RequiresSMT() && !host.HasSMT {
		log.Error("selected config requires SMT but host reports one thread per core",
			"config", string(cfg), "instance_type", *instanceType,
			"hint", "m8a.metal-48xl (AMD) has no SMT; use config A, B, or D")
		os.Exit(1)
	}

	m := metrics.New()
	m.SetTopologyInfo(*instanceType, string(cfg), host.NodeCount, host.PhysicalCores,
		host.ThreadsPerCore, host.HasSMT, string(host.CacheMode))

	// Serve metrics immediately so scrapers see the process even during pool warm-up.
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info("metrics endpoint listening", "addr", *metricsAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server error", "err", err)
		}
	}()

	// Duties 2 & 3: build placement plan, establish parent cgroup hierarchy, launch
	// jailed microVMs.
	ocfg := orchestrator.DefaultConfig()
	ocfg.PlacementConfig = cfg
	ocfg.CgroupName = *cgroupName
	ocfg.JailerBin = *jailerBin
	ocfg.FirecrackerBin = *firecrackerBin
	ocfg.KernelPath = *kernelPath
	ocfg.RootfsPath = *rootfsPath
	ocfg.VCPUCount = *vcpus
	ocfg.MemSizeMiB = *memMiB

	orch := orchestrator.New(ocfg, host.Topology, log)

	plan, err := orch.SetupHierarchy(*density)
	if err != nil {
		log.Error("failed to establish cgroup hierarchy", "err", err)
		_ = srv.Close()
		os.Exit(1)
	}
	log.Info("placement plan built",
		"config", string(plan.Config),
		"planned_vms", len(plan.VMs),
		"parent_cpuset", plan.ParentCPUSet,
	)

	// Context cancelled on the first signal, so in-flight launches unwind.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Cap the warm pool at the planned density; asking for more warm VMs than the plan
	// holds slots for is a configuration mistake worth surfacing.
	target := *warmPoolSize
	if target > len(plan.VMs) {
		log.Warn("warm-pool-size exceeds planned density; capping",
			"requested", target, "planned", len(plan.VMs))
		target = len(plan.VMs)
	}

	// Duty 4: warm pool for dispatch.
	pool := warmpool.New(orch, target, log, m)
	if err := pool.Start(ctx); err != nil && ctx.Err() == nil {
		log.Error("warm pool failed to start", "err", err)
	}

	log.Info("host-agent ready", "warm_pool", pool.Size(), "in_use", pool.InUse())

	// Block until a shutdown signal arrives.
	<-ctx.Done()
	log.Info("shutdown signal received; draining")

	// Graceful shutdown: drain the pool (stops every microVM), then tear down the cgroup
	// hierarchy so the node is left clean. Use a fresh context because ctx is already
	// cancelled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool.Close()
	if err := orch.Teardown(); err != nil {
		log.Error("cgroup teardown reported an error", "err", err)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("metrics server shutdown error", "err", err)
	}
	log.Info("host-agent stopped")
}

func validConfig(c placement.Config) bool {
	for _, v := range placement.AllConfigs {
		if v == c {
			return true
		}
	}
	return false
}
