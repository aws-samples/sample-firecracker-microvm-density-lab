#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Validate that a host is ready to run dense Firecracker microVM workloads.
#
# Read-only: makes no changes. Run after provision-host.sh and a reboot, and again
# at the start of a lab session to confirm the instance is in the expected state.
#
# Exit codes:
#   0  all checks passed
#   1  one or more required checks failed
#   2  passed with warnings
#
# Usage:
#   ./validate-host.sh [--verbose]
#
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

VERBOSE=0
[[ "${1:-}" == "--verbose" ]] && VERBOSE=1

FAILURES=0
WARNINGS=0

check()  { if "$@"; then return 0; else FAILURES=$((FAILURES + 1)); return 1; fi }
soft()   { WARNINGS=$((WARNINGS + 1)); warn "$*"; }

# ---------------------------------------------------------------------------
section "Instance"
# ---------------------------------------------------------------------------
instance_type="$(imds_get meta-data/instance-type 2>/dev/null || echo unknown)"
if [[ "$instance_type" == unknown ]]; then
  soft "could not read instance type from IMDS"
else
  if [[ "$instance_type" == *.metal* ]]; then
    ok "instance type: ${instance_type}"
  else
    err "instance type ${instance_type} is not bare metal"
    FAILURES=$((FAILURES + 1))
  fi
fi

vendor="$(cpu_vendor)"
cores="$(physical_cores)"
threads="$(threads_per_core)"
nodes="$(numa_nodes)"

ok "CPU vendor: ${vendor}"
ok "physical cores: ${cores}, threads per core: ${threads}"
ok "NUMA nodes: ${nodes}"

# ---------------------------------------------------------------------------
section "KVM"
# ---------------------------------------------------------------------------
if [[ -e /dev/kvm ]]; then
  ok "/dev/kvm present"
  if [[ -r /dev/kvm && -w /dev/kvm ]]; then
    ok "/dev/kvm is read-write"
  else
    soft "/dev/kvm is not read-write for the current user (uid ${EUID})"
  fi
else
  err "/dev/kvm missing. Not a metal instance, or KVM modules are not loaded."
  FAILURES=$((FAILURES + 1))
fi

if lsmod 2>/dev/null | grep -qE '^kvm(_intel|_amd)?\s'; then
  ok "KVM modules loaded: $(lsmod | awk '/^kvm/ {printf "%s ", $1}')"
else
  soft "KVM modules not visible in lsmod (may be built into the kernel)"
fi

nx="/sys/module/kvm/parameters/nx_huge_pages"
if [[ -r "$nx" ]]; then
  nx_val="$(cat "$nx")"
  if [[ "$nx_val" == "never" ]]; then
    ok "kvm nx_huge_pages=never (Linux 6.1 KVM_CREATE_VM regression mitigated)"
  else
    soft "kvm nx_huge_pages=${nx_val}; microVM creation rate may be reduced on Linux 6.1+"
  fi
fi

# ---------------------------------------------------------------------------
section "Firecracker"
# ---------------------------------------------------------------------------
for bin in firecracker jailer; do
  if command -v "$bin" >/dev/null 2>&1; then
    ok "$bin: $("$bin" --version 2>/dev/null | head -1)"
  else
    err "$bin not found in PATH"
    FAILURES=$((FAILURES + 1))
  fi
done

# Snapshots are tied to a specific Firecracker version. A mismatch against the
# pinned value means existing snapshots will not resume.
if command -v firecracker >/dev/null 2>&1; then
  actual="$(firecracker --version 2>/dev/null | head -1 | grep -oE 'v?[0-9]+\.[0-9]+\.[0-9]+' || echo unknown)"
  expected="${FIRECRACKER_VERSION#v}"
  if [[ "${actual#v}" != "$expected" ]]; then
    soft "Firecracker ${actual} does not match pinned ${FIRECRACKER_VERSION}; snapshots will not be portable"
  fi
fi

if [[ -d "$JAILER_CHROOT_BASE" ]]; then
  perms="$(stat -c '%a %U:%G' "$JAILER_CHROOT_BASE" 2>/dev/null || echo unknown)"
  ok "jailer chroot base ${JAILER_CHROOT_BASE} (${perms})"
  # The jailer trusts its inputs, so a world-writable chroot base is a real problem.
  if [[ "$(stat -c '%a' "$JAILER_CHROOT_BASE" 2>/dev/null)" =~ [2367]$ ]]; then
    err "${JAILER_CHROOT_BASE} is world-writable. The jailer treats its paths as trusted."
    FAILURES=$((FAILURES + 1))
  fi
else
  err "jailer chroot base ${JAILER_CHROOT_BASE} does not exist"
  FAILURES=$((FAILURES + 1))
fi

if [[ -f "${ARTIFACT_DIR}/vmlinux" ]]; then
  ok "guest kernel: ${ARTIFACT_DIR}/vmlinux ($(du -h "${ARTIFACT_DIR}/vmlinux" | cut -f1))"
else
  soft "no guest kernel at ${ARTIFACT_DIR}/vmlinux; run build-guest-kernel.sh"
fi

# ---------------------------------------------------------------------------
section "Seccomp"
# ---------------------------------------------------------------------------
# Firecracker's seccomp-BPF filter is the primary defense between a compromised
# VMM and host syscalls, and it is on by default. The launcher in this repo never
# forwards flags to Firecracker (asserted by its own tests), so the only way to
# lose the filter on a host is someone starting Firecracker by hand with
# --no-seccomp. Treat any such running process as a hard failure.
no_seccomp_pids=""
for pid in $(pgrep -x firecracker 2>/dev/null || true); do
  if tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null | grep -q -- '--no-seccomp'; then
    no_seccomp_pids="${no_seccomp_pids} ${pid}"
  fi
done
if [[ -n "$no_seccomp_pids" ]]; then
  err "firecracker running with --no-seccomp (pid(s):${no_seccomp_pids}). The seccomp filter is mandatory for untrusted workloads."
  FAILURES=$((FAILURES + 1))
else
  ok "no running firecracker process disables its seccomp filter"
fi
# Independently confirm the filter is active on live VMMs: Seccomp: 2 in
# /proc/<pid>/status means a filter-mode BPF program is installed.
for pid in $(pgrep -x firecracker 2>/dev/null || true); do
  mode="$(awk '/^Seccomp:/ {print $2}' "/proc/${pid}/status" 2>/dev/null || echo unknown)"
  if [[ "$mode" == "2" ]]; then
    [[ $VERBOSE -eq 1 ]] && ok "  pid ${pid}: seccomp filter mode active"
  elif [[ "$mode" != "unknown" ]]; then
    err "firecracker pid ${pid} has Seccomp: ${mode} (expected 2 = filter mode)"
    FAILURES=$((FAILURES + 1))
  fi
done

# ---------------------------------------------------------------------------
section "cgroup v2"
# ---------------------------------------------------------------------------
if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
  ok "cgroup v2 unified hierarchy active"
  controllers="$(cat /sys/fs/cgroup/cgroup.controllers)"
  for c in cpuset cpu memory; do
    if grep -qw "$c" <<<"$controllers"; then
      ok "  controller: $c"
    else
      err "  controller missing: $c"
      FAILURES=$((FAILURES + 1))
    fi
  done

  # cpuset.cpus.partition is owned by the parent cgroup and is not delegatable,
  # so the placement agent must set it from the parent. Its presence here confirms
  # the kernel supports the interface the agent depends on.
  if [[ -e /sys/fs/cgroup/cgroup.subtree_control ]]; then
    ok "  cgroup.subtree_control present (delegation possible)"
  fi
else
  err "cgroup v2 not active. Firecracker needs v2: v1 has high snapshot restore latency."
  FAILURES=$((FAILURES + 1))
fi

# ---------------------------------------------------------------------------
section "Memory hygiene"
# ---------------------------------------------------------------------------
if grep -q '/dev' /proc/swaps 2>/dev/null; then
  err "swap is active. Guest memory could be written to host storage."
  FAILURES=$((FAILURES + 1))
else
  ok "no active swap"
fi

# Absent KSM is better than disabled KSM: on Amazon Linux 2023, CONFIG_KSM is unset,
# so it cannot be turned on at all.
ksm=/sys/kernel/mm/ksm/run
if [[ ! -d /sys/kernel/mm/ksm ]]; then
  ok "KSM not compiled into this kernel; cannot be enabled"
elif [[ -r "$ksm" ]]; then
  if [[ "$(cat "$ksm")" == "0" ]]; then
    ok "KSM disabled"
  else
    err "KSM is enabled. Page deduplication across microVMs is a side-channel risk."
    FAILURES=$((FAILURES + 1))
  fi
else
  soft "/sys/kernel/mm/ksm exists but run is unreadable; check KSM state manually"
fi

# ---------------------------------------------------------------------------
section "SMT"
# ---------------------------------------------------------------------------
# SMT is a tuning parameter on Intel metal and absent on AMD metal. Both are valid;
# this reports which situation the host is in so the lab config can be chosen.
state="$(smt_state)"
case "$state" in
  on)
    ok "SMT on (${threads} threads per core). All lab configs available, including C, E, F."
    ;;
  off)
    ok "SMT off. Config F active; re-enable via /sys/devices/system/cpu/smt/control for C and E."
    ;;
  forceoff)
    soft "SMT is forceoff and cannot be changed at runtime. Configs C and E are not runnable on this host."
    ;;
  notsupported)
    ok "SMT not supported by this processor (expected on AMD metal, e.g. m8a)."
    ok "  Every vCPU is a physical core. Configs C, E, and F do not apply."
    ;;
  *)
    soft "could not read SMT state (${state})"
    ;;
esac

# ---------------------------------------------------------------------------
section "Kernel command line"
# ---------------------------------------------------------------------------
if [[ -r /proc/cmdline ]]; then
  cmdline="$(cat /proc/cmdline)"
  [[ $VERBOSE -eq 1 ]] && printf '  %s\n' "$cmdline"

  for arg in quiet loglevel=1; do
    if grep -qw -- "$arg" <<<"$cmdline"; then
      ok "kernel arg present: ${arg}"
    else
      soft "kernel arg missing: ${arg} (console logging measurably slows snapshot restore)"
    fi
  done
else
  soft "/proc/cmdline not readable; cannot verify kernel arguments"
fi

# ---------------------------------------------------------------------------
section "Limits"
# ---------------------------------------------------------------------------
nofile="$(ulimit -n)"
# Each microVM needs several descriptors (API socket, memory file, tap, vsock).
# 150 to 180 VMs makes the common 1024 default far too low.
if [[ "$nofile" == "unlimited" ]] || (( nofile >= 65536 )); then
  ok "open file limit (this shell): ${nofile}"
else
  soft "open file limit ${nofile} is low for 150+ microVMs; log out and back in to pick up new limits"
fi

# The shell limit above comes from PAM. The host agent will run as a systemd service,
# which ignores limits.d entirely, so check the manager default separately. Amazon
# Linux 2023 defaults to 65535, which is the limit that actually binds in production.
sd_nofile="$(systemctl show --property=DefaultLimitNOFILE --value 2>/dev/null || echo unknown)"
if [[ "$sd_nofile" == "unknown" ]]; then
  soft "could not read systemd DefaultLimitNOFILE"
elif (( sd_nofile >= 1048576 )); then
  ok "systemd DefaultLimitNOFILE: ${sd_nofile}"
else
  soft "systemd DefaultLimitNOFILE is ${sd_nofile}; services (including the host agent) are capped here regardless of limits.d"
fi

file_max="$(cat /proc/sys/fs/file-max 2>/dev/null || echo 0)"
(( file_max >= 1000000 )) && ok "fs.file-max: ${file_max}" \
  || soft "fs.file-max ${file_max} may be low at density"

# ---------------------------------------------------------------------------
section "Network filtering"
# ---------------------------------------------------------------------------
# Guests must not reach the host's instance metadata service.
imds_blocked=0
if command -v nft >/dev/null 2>&1 && \
   nft list ruleset 2>/dev/null | grep -q '169.254.169.254'; then
  ok "IMDS drop rule present (nft)"
  imds_blocked=1
elif command -v iptables >/dev/null 2>&1 && \
     iptables -S 2>/dev/null | grep -q '169.254.169.254'; then
  ok "IMDS drop rule present (iptables)"
  imds_blocked=1
fi
(( imds_blocked )) || soft "no IMDS drop rule for tap devices; guests may reach instance metadata"

# ---------------------------------------------------------------------------
section "Result"
# ---------------------------------------------------------------------------
printf '\n'
if (( FAILURES > 0 )); then
  err "${FAILURES} required check(s) failed, ${WARNINGS} warning(s)"
  printf '\nHost is NOT ready. Address the failures above.\n\n'
  exit 1
elif (( WARNINGS > 0 )); then
  warn "all required checks passed, with ${WARNINGS} warning(s)"
  printf '\nHost is usable. Review warnings before recording measurements.\n\n'
  exit 2
else
  ok "all checks passed"
  printf '\nHost is ready.\n\n'
  exit 0
fi
