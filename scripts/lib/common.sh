#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# Shared helpers for host provisioning and validation scripts.
# Source this file; do not execute it.

# Pinned artifact versions. Snapshots require a fixed Firecracker version, so
# changing FIRECRACKER_VERSION invalidates every previously captured snapshot.
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.16.1}"

# Guest kernel. See docs/kernel-policy.md in the Firecracker repo for the support
# matrix. Note that guest v6.1 support ends 2026-09-02, so new work should target
# v6.18 (supported from Firecracker v1.16.1, through 2028-06-01).
GUEST_KERNEL_VERSION="${GUEST_KERNEL_VERSION:-6.18}"

FIRECRACKER_REPO="firecracker-microvm/firecracker"

# --- Guest kernel supply-chain pins -------------------------------------------
#
# The guest kernel is baked into every microVM on every host, so its source is
# verified before it is built. Pins live here, next to the version they pin.
#
# sha256 of linux-<version>.tar.xz, as published in the canonical checksum file
# https://www.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc. When bumping
# GUEST_KERNEL_VERSION, update this from that file (it is served from
# www.kernel.org, a separate origin from the cdn.kernel.org download host).
# Leave empty to fall back to fetching sha256sums.asc at build time.
guest_kernel_sha256() {
  case "$1" in
    6.18) echo "9106a4605da9e31ff17659d958782b815f9591ab308d03b0ee21aad6c7dced4b" ;;
    *)    echo "" ;;
  esac
}

# Immutable ref (commit SHA) in FIRECRACKER_REPO from which the upstream guest
# kernel configs are fetched. A branch name like "main" is a mutable reference:
# what it serves can change between two builds, silently altering the kernel
# configuration inside every microVM. Bump deliberately, and update the config
# checksums below to match.
FIRECRACKER_GUEST_CONFIG_REF="${FIRECRACKER_GUEST_CONFIG_REF:-9157d2e973bc59aaecce55976a695b669a611c69}"

# sha256 of microvm-kernel-ci-<arch>-<version>.config at FIRECRACKER_GUEST_CONFIG_REF.
guest_config_sha256() {
  case "$1" in
    x86_64-6.18)  echo "ba22401a0c7292a4c024ebcd10a562d4a1f1bfd2faed671406d3b159c0cf5215" ;;
    aarch64-6.18) echo "35cce8e8b754a84523ca20dc068fc3f8f03a15523be36a8623bad54e11972e2a" ;;
    *)            echo "" ;;
  esac
}

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
ARTIFACT_DIR="${ARTIFACT_DIR:-/opt/firecracker}"
JAILER_CHROOT_BASE="${JAILER_CHROOT_BASE:-/srv/jailer}"

# Cores reserved for the host agent and OS, kept outside the microVM pool.
HOST_RESERVE_CORES="${HOST_RESERVE_CORES:-4}"

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[0;33m'; BOLD=$'\033[1m'; RESET=$'\033[0m'

log()   { printf '%s[ .. ]%s %s\n' "$BOLD" "$RESET" "$*"; }
ok()    { printf '%s[ OK ]%s %s\n' "$GREEN" "$RESET" "$*"; }
warn()  { printf '%s[WARN]%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
err()   { printf '%s[FAIL]%s %s\n' "$RED" "$RESET" "$*" >&2; }
die()   { err "$*"; exit 1; }

section() {
  printf '\n%s=== %s ===%s\n' "$BOLD" "$*" "$RESET"
}

require_root() {
  [[ ${EUID} -eq 0 ]] || die "must run as root (try: sudo $0)"
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

# Detect the package manager. Amazon Linux 2023 and Fedora-likes use dnf;
# Debian and Ubuntu use apt-get.
#
# Returns 1 rather than calling die(), so callers can supply a fallback. Calling
# die() here would exit(1) inside a command substitution, which under `set -e`
# terminates the caller before any `||` fallback can run.
detect_pkg_mgr() {
  if command -v dnf >/dev/null 2>&1; then echo dnf; return 0
  elif command -v apt-get >/dev/null 2>&1; then echo apt; return 0
  fi
  return 1
}

# Guard against running the provisioning script on a virtualized instance.
# Firecracker needs /dev/kvm, which on EC2 requires a .metal instance because
# nested virtualization is not offered.
assert_bare_metal() {
  local instance_type
  instance_type="$(imds_get meta-data/instance-type 2>/dev/null || echo unknown)"

  if [[ "${instance_type}" == unknown ]]; then
    warn "could not read instance type from IMDS; skipping bare-metal check"
    return 0
  fi

  if [[ "${instance_type}" != *.metal* ]]; then
    die "instance type ${instance_type} is not bare metal. Firecracker requires /dev/kvm, which needs a .metal instance."
  fi
  ok "instance type ${instance_type} is bare metal"
}

# Fetch a path from IMDSv2. Token-based, since IMDSv1 may be disabled.
imds_get() {
  local path="$1" token
  token="$(curl -sf -X PUT "http://169.254.169.254/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 60" 2>/dev/null)" || return 1
  curl -sf -H "X-aws-ec2-metadata-token: ${token}" \
    "http://169.254.169.254/latest/${path}" 2>/dev/null
}

cpu_vendor() {
  awk -F': ' '/^vendor_id/ {print $2; exit}' /proc/cpuinfo
}

# Firecracker release assets use x86_64 and aarch64. Normalize, because `uname -m`
# reports arm64 on some systems (notably macOS), which would produce a 404 when
# building the download URL.
normalize_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo x86_64 ;;
    aarch64|arm64) echo aarch64 ;;
    *)             echo "$(uname -m)" ;;
  esac
}

# Reads SMT state from sysfs. Possible values: on, off, forceoff, notsupported.
# notsupported is expected on AMD EC2 metal (for example m8a.metal-48xl reports
# 192 cores with 1 thread each), where there is no SMT to configure.
smt_state() {
  local f=/sys/devices/system/cpu/smt/control
  [[ -r "$f" ]] && cat "$f" || echo "unavailable"
}

# Count distinct physical cores. `lscpu -p=CORE` emits one row per *logical* CPU, so
# the rows must be deduplicated: on a host with SMT, counting rows returns the vCPU
# count instead. Getting this wrong corrupts every core-allocation calculation.
physical_cores() { lscpu -p=CORE | grep -v '^#' | sort -u | wc -l | tr -d ' '; }
threads_per_core() { lscpu | awk -F': +' '/^Thread\(s\) per core/ {print $2}'; }
numa_nodes() { lscpu | awk -F': +' '/^NUMA node\(s\)/ {print $2}'; }
