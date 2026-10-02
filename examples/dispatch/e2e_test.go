// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// End-to-end integration test for the dispatch example.
//
// This test is gated so it only runs on a provisioned metal host with a live microVM. It skips
// (rather than fails) everywhere else, so `go test ./...` stays green on a laptop or in CI that
// has no hardware. The build itself is always compiled, which is what makes it useful as a
// regression guard on the protocol code even where it cannot run.
//
// # Preconditions to actually run it
//
//  1. A prepared metal host: m8i.metal-48xl or m8a.metal-48xl, provisioned by
//     ../../scripts/provision-host.sh (cgroup v2, jailer + firecracker v1.16.1, guest kernel
//     6.18, a rootfs.ext4 containing the guest agent). Validate with
//     ../../scripts/validate-host.sh.
//  2. A running, booted microVM whose guest agent is answering on vsock. Two easy ways:
//     - start the host agent (it boots a warm pool):
//     sudo ../../host-agent/host-agent -config=D -density=8 -warm-pool-size=2 \
//     -kernel=/opt/firecracker/vmlinux -rootfs=/opt/firecracker/rootfs.ext4
//     then point the test at one microVM's socket, e.g. /srv/jailer/firecracker/vm0/root/v.sock
//     - or launch a single microVM with the lab tooling.
//  3. Export the socket path and opt in:
//     export FC_E2E_VSOCK=/srv/jailer/firecracker/vm0/root/v.sock
//     go test -run TestE2E -v ./...
//
// The gate is the FC_E2E_VSOCK environment variable AND the socket existing. Both must hold or
// the test skips with an explanatory message.
package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// requireVsock returns the gated socket path or skips the test. Centralised so every subtest
// prints the same precondition message.
func requireVsock(t *testing.T) string {
	t.Helper()
	path := os.Getenv("FC_E2E_VSOCK")
	if path == "" {
		t.Skip("FC_E2E_VSOCK not set: this end-to-end test only runs on a provisioned metal host " +
			"with a live microVM. See the package comment for preconditions.")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("FC_E2E_VSOCK=%q does not exist (%v): boot a microVM first (host agent warm pool "+
			"or lab launch), then point the test at its <chroot>/v.sock.", path, err)
	}
	return path
}

// TestE2E_Ping is the fastest liveness signal: the guest agent must answer PING with OK pong.
func TestE2E_Ping(t *testing.T) {
	vsock := requireVsock(t)

	reply, err := agentRequest(vsock, "PING", 5*time.Second)
	if err != nil {
		t.Fatalf("PING over vsock failed: %v", err)
	}
	if reply != "OK pong" {
		t.Fatalf("PING: got %q, want %q", reply, "OK pong")
	}
}

// TestE2E_RunSandbox dispatches the agentic-sandbox-shaped workload and validates the OKS reply
// end to end: the fields echo the request and the timings are present and non-empty.
func TestE2E_RunSandbox(t *testing.T) {
	vsock := requireVsock(t)

	const (
		residentKiB = 65536
		steps       = 20
		writeKiB    = 256
	)
	request, err := buildRequest("sandbox", residentKiB, steps, writeKiB, 0, 0)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	reply, err := agentRequest(vsock, request, 60*time.Second)
	if err != nil {
		t.Fatalf("RUN_SANDBOX over vsock failed: %v", err)
	}

	// OKS <resident_kib> <steps> <write_kib> <total_us> <spawn_us> <write_us> <spawns_ok> <checksum>
	fields := strings.Fields(reply)
	if len(fields) != 9 || fields[0] != "OKS" {
		t.Fatalf("RUN_SANDBOX: expected a 9-field OKS reply, got %q", reply)
	}
	// The guest echoes the request parameters back in the first three fields; assert they match
	// so a misrouted or mismatched reply is caught rather than silently accepted.
	for i, want := range []string{"65536", "20", "256"} {
		if fields[i+1] != want {
			t.Errorf("RUN_SANDBOX echoed field %d = %q, want %q", i+1, fields[i+1], want)
		}
	}
	if fields[4] == "0" {
		t.Errorf("RUN_SANDBOX total_us is 0, expected the workload to take measurable time; reply=%q", reply)
	}
}
