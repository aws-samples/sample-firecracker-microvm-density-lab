// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVMM is an HTTP server on a Unix socket that mimics the subset of the Firecracker API
// this client uses. Testing against a real socket exercises the custom transport, which is
// the part most likely to be wrong; a mocked http.Client would skip it entirely.
type fakeVMM struct {
	mu       sync.Mutex
	requests []recorded
	// status overrides the response code for a given path.
	status map[string]int
	// fault is returned as a Firecracker fault_message body when status is an error.
	fault map[string]string
	state string
}

type recorded struct {
	method string
	path   string
	body   string
}

func newFakeVMM(t *testing.T) (*fakeVMM, string) {
	t.Helper()

	// Socket paths are limited to about 104 bytes on macOS, so keep the directory short
	// rather than using the long default temp path.
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s")

	f := &fakeVMM{
		status: map[string]int{},
		fault:  map[string]string{},
		state:  "Not started",
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}

	srv := &http.Server{Handler: f}
	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	})

	return f, sock
}

func (f *fakeVMM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.requests = append(f.requests, recorded{r.Method, r.URL.Path, string(body)})
	status, hasStatus := f.status[r.URL.Path]
	fault := f.fault[r.URL.Path]
	state := f.state
	f.mu.Unlock()

	if hasStatus && status >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if fault != "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": fault})
		}
		return
	}

	if r.Method == http.MethodGet && r.URL.Path == "/" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(InstanceInfo{
			ID: "lab", State: state, VMMVersion: "1.16.1", AppName: "Firecracker",
		})
		return
	}

	// Firecracker answers 204 for successful mutations.
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeVMM) got() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recorded, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeVMM) failPath(path string, status int, fault string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[path] = status
	f.fault[path] = fault
}

func TestConfigureAndStartSequence(t *testing.T) {
	fake, sock := newFakeVMM(t)
	c := New(sock)
	ctx := context.Background()

	if err := c.PutMachineConfig(ctx, MachineConfig{VCPUCount: 1, MemSizeMiB: 128}); err != nil {
		t.Fatalf("PutMachineConfig: %v", err)
	}
	if err := c.PutBootSource(ctx, BootSource{
		KernelImagePath: "/opt/firecracker/vmlinux",
		BootArgs:        "console= 8250.nr_uarts=0 reboot=k panic=1 pci=off",
	}); err != nil {
		t.Fatalf("PutBootSource: %v", err)
	}
	if err := c.PutDrive(ctx, Drive{
		DriveID: "rootfs", PathOnHost: "/opt/firecracker/rootfs.ext4",
		IsRootDevice: true, IsReadOnly: true,
	}); err != nil {
		t.Fatalf("PutDrive: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := fake.got()
	want := []struct{ method, path string }{
		{"PUT", "/machine-config"},
		{"PUT", "/boot-source"},
		{"PUT", "/drives/rootfs"},
		{"PUT", "/actions"},
	}
	if len(got) != len(want) {
		t.Fatalf("recorded %d requests, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].method != w.method || got[i].path != w.path {
			t.Errorf("request %d = %s %s, want %s %s", i, got[i].method, got[i].path, w.method, w.path)
		}
	}

	// The drive id must appear in the path, not only the body: Firecracker keys drives by
	// URL segment and a mismatch silently creates a second device.
	if !strings.Contains(got[2].path, "rootfs") {
		t.Errorf("drive path %q does not contain the drive id", got[2].path)
	}
	// InstanceStart must be the action type, or the VM is configured but never boots.
	if !strings.Contains(got[3].body, "InstanceStart") {
		t.Errorf("action body %q does not request InstanceStart", got[3].body)
	}
}

func TestMachineConfigOmitsEmptyOptionals(t *testing.T) {
	fake, sock := newFakeVMM(t)
	c := New(sock)

	if err := c.PutMachineConfig(context.Background(), MachineConfig{VCPUCount: 1, MemSizeMiB: 128}); err != nil {
		t.Fatal(err)
	}
	body := fake.got()[0].body

	// An empty cpu_template must not be sent: Firecracker rejects "" as an invalid
	// template rather than treating it as unset.
	if strings.Contains(body, "cpu_template") {
		t.Errorf("body %q includes cpu_template when unset", body)
	}
	for _, required := range []string{"vcpu_count", "mem_size_mib"} {
		if !strings.Contains(body, required) {
			t.Errorf("body %q missing required field %s", body, required)
		}
	}
}

func TestInfoReportsState(t *testing.T) {
	fake, sock := newFakeVMM(t)
	fake.mu.Lock()
	fake.state = "Running"
	fake.mu.Unlock()

	info, err := New(sock).Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.State != "Running" {
		t.Errorf("State = %q, want Running", info.State)
	}
	if info.VMMVersion != "1.16.1" {
		t.Errorf("VMMVersion = %q, want 1.16.1", info.VMMVersion)
	}
}

// A rejected request must surface Firecracker's fault message. Without it, debugging a
// failed launch means guessing from a status code.
func TestFaultMessageSurfaced(t *testing.T) {
	fake, sock := newFakeVMM(t)
	fake.failPath("/boot-source", http.StatusBadRequest,
		"Cannot open kernel file: No such file or directory (os error 2)")

	err := New(sock).PutBootSource(context.Background(), BootSource{KernelImagePath: "/nope"})
	if err == nil {
		t.Fatal("PutBootSource on a rejected request returned nil error")
	}
	if !strings.Contains(err.Error(), "Cannot open kernel file") {
		t.Errorf("error %q does not include the fault message", err)
	}
	if !strings.Contains(err.Error(), "/boot-source") {
		t.Errorf("error %q does not identify which endpoint failed", err)
	}
}

func TestErrorWithoutFaultBodyStillIdentifiesEndpoint(t *testing.T) {
	fake, sock := newFakeVMM(t)
	fake.failPath("/machine-config", http.StatusInternalServerError, "")

	err := New(sock).PutMachineConfig(context.Background(), MachineConfig{VCPUCount: 1, MemSizeMiB: 128})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "/machine-config") || !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q should name the endpoint and status", err)
	}
}

func TestWaitReadySucceedsOnceServing(t *testing.T) {
	_, sock := newFakeVMM(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := New(sock).WaitReady(ctx); err != nil {
		t.Errorf("WaitReady on a live socket: %v", err)
	}
}

// A socket that never appears must time out with a useful message rather than hang, since
// at density a stuck launch would stall the whole sweep.
func TestWaitReadyTimesOutOnMissingSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err = New(filepath.Join(dir, "absent")).WaitReady(ctx)
	if err == nil {
		t.Fatal("WaitReady on a missing socket returned nil")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("error %q does not name the socket path", err)
	}
}

func TestContextCancellationIsRespected(t *testing.T) {
	_, sock := newFakeVMM(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	if err := New(sock).Start(ctx); err == nil {
		t.Error("Start with a cancelled context returned nil error")
	}
}
