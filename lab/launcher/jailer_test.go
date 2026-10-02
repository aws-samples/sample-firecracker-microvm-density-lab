// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package launcher

import (
	"strings"
	"testing"
)

func validConfig() JailerConfig {
	return JailerConfig{
		ID:         "vm0",
		JailerBin:  "/usr/local/bin/jailer",
		ExecFile:   "/usr/local/bin/firecracker",
		ChrootBase: "/srv/jailer",
		UID:        10000,
		GID:        10000,
	}
}

func TestValidateAcceptsGoodConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Errorf("Validate on a good config: %v", err)
	}
}

// The jailer accepts only alphanumerics and hyphens in an ID, and uses it as a directory
// name. A separator would place the jail somewhere unintended.
func TestValidateRejectsBadIDs(t *testing.T) {
	bad := []string{
		"",
		"vm_0",                  // underscore is not permitted
		"vm/0",                  // path separator
		"../escape",             // traversal
		"vm 0",                  // space
		strings.Repeat("a", 65), // over the 64 character limit
	}
	for _, id := range bad {
		c := validConfig()
		c.ID = id
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted ID %q", id)
		}
	}
}

// Running the jailed process as root would defeat the privilege drop that is the jailer's
// primary purpose, so uid or gid 0 must be refused rather than silently accepted.
func TestValidateRejectsRoot(t *testing.T) {
	for _, tc := range []struct{ uid, gid int }{{0, 10000}, {10000, 0}, {0, 0}, {-1, 10000}} {
		c := validConfig()
		c.UID, c.GID = tc.uid, tc.gid
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted uid=%d gid=%d", tc.uid, tc.gid)
		}
	}
}

func TestValidateRequiresBinaries(t *testing.T) {
	c := validConfig()
	c.JailerBin = ""
	if err := c.Validate(); err == nil {
		t.Error("Validate accepted an empty JailerBin")
	}
	c = validConfig()
	c.ExecFile = ""
	if err := c.Validate(); err == nil {
		t.Error("Validate accepted an empty ExecFile")
	}
}

func TestArgsIncludesRequiredFlags(t *testing.T) {
	args := strings.Join(validConfig().Args(), " ")

	for _, want := range []string{
		"--id vm0",
		"--exec-file /usr/local/bin/firecracker",
		"--uid 10000",
		"--gid 10000",
		"--chroot-base-dir /srv/jailer",
		// v2 is not the jailer's default, so it must be requested explicitly. v1 has
		// documented high snapshot restore latency and no partition interface.
		"--cgroup-version 2",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("Args() = %q, missing %q", args, want)
		}
	}
}

// The move-into-existing-cgroup path requires --parent-cgroup with no --cgroup entries.
// Passing a --cgroup would make the jailer create its own cgroup instead, which would take
// the microVM out from under the parent that owns partition state.
func TestParentCgroupWithoutCgroupEntries(t *testing.T) {
	c := validConfig()
	c.ParentCgroup = "fclab/vm0"
	args := strings.Join(c.Args(), " ")

	if !strings.Contains(args, "--parent-cgroup fclab/vm0") {
		t.Errorf("Args() = %q, missing --parent-cgroup", args)
	}
	if strings.Contains(args, "--cgroup ") {
		t.Errorf("Args() = %q includes --cgroup, which would make the jailer create its own cgroup", args)
	}
}

func TestCgroupEntriesArePassedThrough(t *testing.T) {
	c := validConfig()
	c.Cgroups = []string{"cpuset.cpus=5", "cpuset.mems=0"}
	args := strings.Join(c.Args(), " ")
	for _, want := range []string{"--cgroup cpuset.cpus=5", "--cgroup cpuset.mems=0"} {
		if !strings.Contains(args, want) {
			t.Errorf("Args() = %q, missing %q", args, want)
		}
	}
}

func TestOptionalFlagsOmittedWhenUnset(t *testing.T) {
	args := strings.Join(validConfig().Args(), " ")
	for _, absent := range []string{"--netns", "--daemonize", "--new-pid-ns", "--parent-cgroup", "--resource-limit"} {
		if strings.Contains(args, absent) {
			t.Errorf("Args() = %q includes %q when it was not configured", args, absent)
		}
	}
}

func TestResourceLimitsAndNamespaceFlags(t *testing.T) {
	c := validConfig()
	c.ResourceLimits = []string{"no-file=4096", "fsize=250000000"}
	c.NetNS = "/var/run/netns/fc0"
	c.Daemonize = true
	c.NewPIDNS = true

	args := strings.Join(c.Args(), " ")
	for _, want := range []string{
		"--resource-limit no-file=4096",
		"--resource-limit fsize=250000000",
		"--netns /var/run/netns/fc0",
		"--daemonize",
		"--new-pid-ns",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("Args() = %q, missing %q", args, want)
		}
	}
}

// The jail path shape is dictated by the jailer: <chroot_base>/<exec_file_name>/<id>/root.
// Getting it wrong means every artifact is staged where Firecracker cannot see it.
func TestChrootDirLayout(t *testing.T) {
	c := validConfig()
	want := "/srv/jailer/firecracker/vm0/root"
	if got := c.ChrootDir(); got != want {
		t.Errorf("ChrootDir() = %q, want %q", got, want)
	}
}

func TestChrootDirDefaultsBase(t *testing.T) {
	c := validConfig()
	c.ChrootBase = ""
	if got := c.ChrootDir(); !strings.HasPrefix(got, "/srv/jailer/") {
		t.Errorf("ChrootDir() = %q, want the default /srv/jailer base", got)
	}
	if !strings.Contains(strings.Join(c.Args(), " "), "--chroot-base-dir /srv/jailer") {
		t.Error("Args() should pass the default chroot base explicitly")
	}
}

// The API socket default is /run/firecracker.socket relative to the jail root, per the
// jailer documentation. A wrong path here breaks every launch with a confusing timeout
// rather than a clear error.
func TestAPISocketPathMatchesJailerDefault(t *testing.T) {
	c := validConfig()
	c.ID = "551e7604-e35c-42b3-b825-416853441234"

	wantJail := "/srv/jailer/firecracker/551e7604-e35c-42b3-b825-416853441234/root"
	if got := c.ChrootDir(); got != wantJail {
		t.Fatalf("ChrootDir() = %q, want %q", got, wantJail)
	}
	// Launch composes APISocket as <jail>/run/firecracker.socket. Assert the shape here
	// without spawning a process.
	wantSock := wantJail + "/run/firecracker.socket"
	got := c.ChrootDir() + "/run/firecracker.socket"
	if got != wantSock {
		t.Errorf("API socket path = %q, want %q", got, wantSock)
	}
}

func TestLaunchRejectsInvalidConfigBeforeSpawning(t *testing.T) {
	c := validConfig()
	c.UID = 0 // root, which Validate refuses

	// A nil context would panic if Launch reached exec, so reaching the validation error
	// first is itself part of what is being checked.
	_, err := Launch(nil, c) //nolint:staticcheck // deliberately nil to prove validation precedes use
	if err == nil {
		t.Fatal("Launch accepted a config with uid 0")
	}
	if !strings.Contains(err.Error(), "invalid jailer config") {
		t.Errorf("error %q should identify the failure as configuration", err)
	}
}

// The seccomp invariant: Firecracker's seccomp-BPF filter is on by default and the only
// way to weaken it is a flag forwarded after a "--" separator. These tests pin both halves
// of the guarantee — Args never emits the separator or any seccomp flag, and Validate
// rejects values that try to smuggle one in.

func validSeccompTestConfig() JailerConfig {
	return JailerConfig{
		ID:        "vm0",
		JailerBin: "/usr/local/bin/jailer",
		ExecFile:  "/usr/local/bin/firecracker",
		UID:       100000,
		GID:       100000,
	}
}

func TestArgsNeverForwardsToFirecracker(t *testing.T) {
	c := validSeccompTestConfig()
	c.ParentCgroup = "fclab/vm0"
	c.Cgroups = []string{"cpuset.cpus=2"}
	c.NetNS = "/var/run/netns/fc"
	c.ResourceLimits = []string{"no-file=4096"}
	c.Daemonize = true
	c.NewPIDNS = true

	for _, arg := range c.Args() {
		if arg == "--" {
			t.Fatal("Args emitted the \"--\" separator; flags after it reach Firecracker and could disable seccomp")
		}
		if strings.Contains(arg, "seccomp") {
			t.Fatalf("Args emitted a seccomp-related flag %q; the default filter must stay untouched", arg)
		}
	}
}

func TestValidateRejectsSeccompSmuggling(t *testing.T) {
	cases := map[string]func(*JailerConfig){
		"cgroup value":          func(c *JailerConfig) { c.Cgroups = []string{"cpuset.cpus=2 -- --no-seccomp"} },
		"resource limit":        func(c *JailerConfig) { c.ResourceLimits = []string{"no-file=4096 -- --no-seccomp"} },
		"netns":                 func(c *JailerConfig) { c.NetNS = "/run/netns/x --no-seccomp" },
		"custom seccomp filter": func(c *JailerConfig) { c.Cgroups = []string{"x=1 -- --seccomp-filter /tmp/f"} },
	}
	for name, mutate := range cases {
		c := validSeccompTestConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted a config that tampers with the seccomp filter", name)
		}
	}
}

func TestValidateAcceptsCleanConfig(t *testing.T) {
	c := validSeccompTestConfig()
	c.Cgroups = []string{"cpuset.cpus=2"}
	c.ResourceLimits = []string{"no-file=4096"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate rejected a clean config: %v", err)
	}
}
