// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// JailerConfig describes one jailed Firecracker process.
type JailerConfig struct {
	// ID is the microVM identifier. The jailer uses it as a directory name and passes it
	// to Firecracker, so it must be alphanumeric with hyphens, at most 64 characters.
	ID string

	JailerBin  string // path to the jailer binary
	ExecFile   string // path to the firecracker binary the jailer will exec
	ChrootBase string // jail root, /srv/jailer by default

	// UID and GID are the unprivileged identity Firecracker runs as. Upstream recommends
	// a distinct pair per microVM so a jailbreak cannot reach a sibling's files.
	UID int
	GID int

	// ParentCgroup places this microVM's cgroup under an existing parent. With cgroup v2
	// and no Cgroups entries, the jailer moves the process into this cgroup instead of
	// creating one, which is what lets the parent own partition state.
	ParentCgroup string

	// Cgroups are <file>=<value> pairs the jailer writes before the VM starts. Leave empty
	// when the parent hierarchy already assigned cpuset values, so the jailer takes the
	// move-into-existing-cgroup path.
	Cgroups []string

	// NetNS is a pre-created network namespace to join.
	NetNS string

	// ResourceLimits are <resource>=<value> pairs, for example "no-file=4096". The jailer
	// defaults no-file to 2048, which is low once a microVM holds an API socket, a memory
	// file, a tap device, and a vsock.
	ResourceLimits []string

	Daemonize bool // detach and redirect stdio to /dev/null
	NewPIDNS  bool // run Firecracker as init in its own PID namespace
}

// Process is a running jailed Firecracker.
type Process struct {
	ID        string
	Cmd       *exec.Cmd
	ChrootDir string // <chroot_base>/<exec_name>/<id>/root
	APISocket string // absolute host path to the API socket
	started   time.Time
}

// Validate checks the configuration before anything is launched.
//
// The jailer treats all its inputs as trusted and will not sanity-check them, so mistakes
// surface as confusing runtime failures rather than clear errors. Checking here is cheaper
// than debugging 180 failed launches.
func (c JailerConfig) Validate() error {
	if c.ID == "" {
		return errors.New("ID must not be empty")
	}
	if len(c.ID) > 64 {
		return fmt.Errorf("ID %q exceeds the jailer's 64 character limit", c.ID)
	}
	for _, r := range c.ID {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return fmt.Errorf("ID %q contains %q; only alphanumerics and hyphens are allowed", c.ID, r)
		}
	}
	if c.JailerBin == "" {
		return errors.New("JailerBin must be set")
	}
	if c.ExecFile == "" {
		return errors.New("ExecFile must be set")
	}
	if c.UID <= 0 || c.GID <= 0 {
		// Running the jailed process as root would defeat the privilege drop that is the
		// jailer's main purpose.
		return fmt.Errorf("UID and GID must be non-root positive values, got %d:%d", c.UID, c.GID)
	}
	// Seccomp assertion: Firecracker installs its seccomp-BPF filter by default, and this
	// launcher relies on that default — Args() deliberately never emits the "--" separator
	// that would forward flags to Firecracker, so nothing here can reach --no-seccomp.
	// The filter is the primary defense between a compromised VMM and host syscalls when
	// the guest runs untrusted code, so disabling it must not be possible by smuggling
	// the flag through a list-valued field. Reject it outright.
	for _, vals := range [][]string{c.Cgroups, c.ResourceLimits, {c.NetNS, c.ChrootBase}} {
		for _, v := range vals {
			if strings.Contains(v, "no-seccomp") || strings.Contains(v, "seccomp-filter") {
				return fmt.Errorf("config value %q attempts to alter Firecracker's seccomp filter; the default filter is mandatory for untrusted workloads", v)
			}
		}
	}
	return nil
}

// Args renders the jailer command line.
//
// Ordering matters: everything after a "--" separator would be forwarded to Firecracker
// rather than consumed by the jailer. Args never emits that separator, which is a security
// invariant, not an omission: Firecracker enables its seccomp-BPF filter by default, and
// the only way to weaken it is to pass --no-seccomp (or a custom --seccomp-filter) after
// "--". With no pass-through, the default filter is always active. Validate() additionally
// rejects any config value that tries to smuggle a seccomp flag in. Tests pin both
// properties; do not add Firecracker argument forwarding without preserving them.
func (c JailerConfig) Args() []string {
	base := c.ChrootBase
	if base == "" {
		base = "/srv/jailer"
	}

	args := []string{
		"--id", c.ID,
		"--exec-file", c.ExecFile,
		"--uid", strconv.Itoa(c.UID),
		"--gid", strconv.Itoa(c.GID),
		"--chroot-base-dir", base,
		// v2 is required: v1 has documented high snapshot restore latency, and the
		// partition interface this experiment depends on does not exist there.
		"--cgroup-version", "2",
	}
	if c.ParentCgroup != "" {
		args = append(args, "--parent-cgroup", c.ParentCgroup)
	}
	for _, cg := range c.Cgroups {
		args = append(args, "--cgroup", cg)
	}
	if c.NetNS != "" {
		args = append(args, "--netns", c.NetNS)
	}
	for _, rl := range c.ResourceLimits {
		args = append(args, "--resource-limit", rl)
	}
	if c.Daemonize {
		args = append(args, "--daemonize")
	}
	if c.NewPIDNS {
		args = append(args, "--new-pid-ns")
	}
	return args
}

// ChrootDir returns the jail root the jailer will create for this microVM.
func (c JailerConfig) ChrootDir() string {
	base := c.ChrootBase
	if base == "" {
		base = "/srv/jailer"
	}
	return filepath.Join(base, filepath.Base(c.ExecFile), c.ID, "root")
}

// PrepareJail creates the jail directory and copies the guest artifacts into it.
//
// Firecracker runs chrooted, so every path it is later given over the API must resolve
// inside the jail. Artifacts have to be copied or hard-linked in beforehand; a path that
// exists on the host but not in the jail produces an opaque "No such file" fault at
// configure time.
func PrepareJail(c JailerConfig, artifacts map[string]string) error {
	dir := c.ChrootDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create jail dir %s: %w", dir, err)
	}
	// The jailed process must own its root to create the API socket there.
	if err := os.Chown(dir, c.UID, c.GID); err != nil {
		return fmt.Errorf("chown jail dir %s to %d:%d: %w", dir, c.UID, c.GID, err)
	}

	for name, src := range artifacts {
		dst := filepath.Join(dir, name)
		// Hard-link rather than copy where possible: at 180 microVMs sharing one rootfs
		// and kernel, copying would multiply disk use and page cache pressure, defeating
		// the shared-page-cache benefit the density model depends on.
		if err := os.Link(src, dst); err != nil {
			if err := copyFile(src, dst); err != nil {
				return fmt.Errorf("stage %s into jail: %w", name, err)
			}
		}
		if err := os.Chown(dst, c.UID, c.GID); err != nil {
			return fmt.Errorf("chown %s: %w", dst, err)
		}
	}
	return nil
}

// Launch starts a jailed Firecracker process.
//
// It returns once the process is spawned, not once the API is answering. Callers should use
// the client's WaitReady before configuring the microVM.
func Launch(ctx context.Context, c JailerConfig) (*Process, error) {
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid jailer config: %w", err)
	}

	// c.JailerBin is an operator-set path; c.Args() renders a fixed jailer flag set from
	// JailerConfig fields validated above (ID charset/length-checked, UID/GID non-root,
	// seccomp flags rejected, no "--" pass-through to Firecracker). Inputs are never
	// guest-controlled, and exec.CommandContext uses execve (no shell).
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, c.JailerBin, c.Args()...)
	// Without daemonize the jailer keeps stdio attached, and a guest that writes to the
	// serial console can then flood the parent's memory. Discard explicitly.
	if !c.Daemonize {
		cmd.Stdout = nil
		cmd.Stderr = nil
	}
	// Put the jailer in its own process group so a signal to the harness does not tear
	// down microVMs mid-measurement.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start jailer for %s: %w", c.ID, err)
	}

	return &Process{
		ID:        c.ID,
		Cmd:       cmd,
		ChrootDir: c.ChrootDir(),
		APISocket: filepath.Join(c.ChrootDir(), "run", "firecracker.socket"),
		started:   time.Now(),
	}, nil
}

// Uptime reports how long since the process was launched.
func (p *Process) Uptime() time.Duration { return time.Since(p.started) }

// Stop terminates the microVM.
//
// SIGKILL is used rather than a graceful shutdown because it is the only reliable stop: a
// guest without an ACPI handler ignores Ctrl+Alt+Del, and a VMM wedged by the documented
// signal-handler deadlock will not respond to anything softer. Upstream's own guidance for
// unresponsive Firecracker processes is SIGKILL.
//
// The Firecracker pid must be killed, not the jailer's. With --daemonize the jailer exits
// once it has spawned Firecracker, so Cmd.Process refers to a process that is already gone.
// Killing that reaps nothing, leaves the VMM running, and makes the cgroup unremovable
// ("device or resource busy") because it still holds a process. The jailer writes the child
// pid into the jail root precisely so callers can find the real target.
func (p *Process) Stop() error {
	var firstErr error

	// Kill the VMM first, using the pid recorded in the jail.
	if pid, err := p.firecrackerPID(); err == nil && pid > 0 {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil &&
			!errors.Is(err, syscall.ESRCH) { // ESRCH: already gone, which is fine
			firstErr = fmt.Errorf("kill firecracker pid %d for %s: %w", pid, p.ID, err)
		} else {
			// Wait briefly for the kernel to release the process, so a following cgroup
			// removal does not race it.
			p.waitForExit(pid, 2*time.Second)
		}
	}

	// Then the jailer, in case it is still around (no --daemonize, or it has not exited).
	if p.Cmd != nil && p.Cmd.Process != nil {
		if err := p.Cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) &&
			!errors.Is(err, syscall.ESRCH) {
			if firstErr == nil {
				firstErr = fmt.Errorf("kill jailer for %s: %w", p.ID, err)
			}
		}
		// Reap so the jailer does not linger as a zombie.
		_, _ = p.Cmd.Process.Wait()
	}

	return firstErr
}

// firecrackerPID reads the pid the jailer recorded for the Firecracker process.
func (p *Process) firecrackerPID() (int, error) {
	raw, err := os.ReadFile(filepath.Join(p.ChrootDir, "firecracker.pid"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(raw)))
}

// waitForExit polls until the process is gone or the timeout elapses.
//
// Signal delivery is asynchronous, so returning immediately after SIGKILL would let a cgroup
// removal run while the process still exists.
func (p *Process) waitForExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Signal 0 checks for existence without delivering anything.
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// PID returns the jailer's process id, or the Firecracker pid recorded in the jail when the
// jailer daemonized.
//
// With --daemonize and without --new-pid-ns the jailer exits while Firecracker keeps
// running, so the jailer's pid is not the VMM's. The jailer writes the child pid into the
// jail root for exactly this case.
func (p *Process) PID() (int, error) {
	if pid, err := p.firecrackerPID(); err == nil {
		return pid, nil
	}
	if p.Cmd != nil && p.Cmd.Process != nil {
		return p.Cmd.Process.Pid, nil
	}
	return 0, errors.New("no pid available")
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o644)
}
