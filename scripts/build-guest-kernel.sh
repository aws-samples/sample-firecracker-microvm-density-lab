#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Build a guest kernel for Firecracker microVMs.
#
# Produces an uncompressed vmlinux from the Firecracker project's recommended guest
# config. vmlinux is preferred over bzImage: a bzImage is decompressed by the guest
# at boot, costing boot time and guest memory, and both costs multiply at density.
#
# Run once during AMI build. The resulting kernel is shared by every microVM on the
# host, so it does not need rebuilding per workload.
#
# Usage:
#   sudo ./build-guest-kernel.sh [--version 6.18] [--config path/to/.config] [--sha256 <hash>]
#
# The source tarball is sha256-verified before building: against --sha256 if given,
# else the pin in lib/common.sh, else the canonical checksum file on www.kernel.org.
# An unverifiable tarball is a hard failure.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

KERNEL_VERSION="${GUEST_KERNEL_VERSION}"
CUSTOM_CONFIG=""
TARBALL_SHA256=""
BUILD_DIR="${BUILD_DIR:-/tmp/guest-kernel-build}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) KERNEL_VERSION="$2"; shift 2 ;;
    --config)  CUSTOM_CONFIG="$2"; shift 2 ;;
    --sha256)  TARBALL_SHA256="$2"; shift 2 ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)         die "unknown argument: $1" ;;
  esac
done

require_root

# Guest kernel support windows are documented in the Firecracker kernel policy.
# v6.1 support ends 2026-09-02, so it is a poor choice for anything with a shelf life.
case "$KERNEL_VERSION" in
  6.18) ok "guest kernel v6.18 (supported from Firecracker v1.16.1 through 2028-06-01)" ;;
  6.1)  warn "guest kernel v6.1 support ends 2026-09-02. Prefer v6.18." ;;
  5.10) warn "guest kernel v5.10 is past its minimum end of support (2024-01-31)." ;;
  *)    warn "guest kernel v${KERNEL_VERSION} is not in the documented support matrix." ;;
esac

section "1. Build dependencies"
PKG_MGR="$(detect_pkg_mgr)"
case "$PKG_MGR" in
  dnf) dnf install -y git make gcc flex bison elfutils-libelf-devel openssl-devel \
         bc perl tar xz ncurses-devel ;;
  apt) DEBIAN_FRONTEND=noninteractive apt-get update -qq
       DEBIAN_FRONTEND=noninteractive apt-get install -y git make gcc flex bison \
         libelf-dev libssl-dev bc perl tar xz-utils libncurses-dev ;;
esac
ok "build dependencies installed"

section "2. Kernel source"
mkdir -p "$BUILD_DIR"
cd "$BUILD_DIR"

# Shallow tarball fetch rather than a full git clone: much faster and the history
# is not needed for a one-off build.
major="${KERNEL_VERSION%%.*}"
tarball="linux-${KERNEL_VERSION}.tar.xz"
src_dir="linux-${KERNEL_VERSION}"

# Verify the tarball before extracting: this kernel is baked into every microVM on
# every host, so an attacker who can tamper with the download path (or a corrupted
# mirror) would otherwise land inside the trusted computing base of the whole fleet.
# Precedence: --sha256 on the command line, then the pin in lib/common.sh, then the
# canonical checksum file on www.kernel.org (a separate origin from the
# cdn.kernel.org download host). No verification source is a hard failure.
verify_kernel_tarball() {
  local expected="$TARBALL_SHA256"
  if [[ -z "$expected" ]]; then
    expected="$(guest_kernel_sha256 "$KERNEL_VERSION")"
  fi
  if [[ -z "$expected" ]]; then
    log "no pinned sha256 for v${KERNEL_VERSION}; fetching the canonical checksum file from www.kernel.org"
    expected="$(curl -sfL "https://www.kernel.org/pub/linux/kernel/v${major}.x/sha256sums.asc" \
      | awk -v t="$tarball" '$2 == t {print $1; exit}')" || true
  fi
  [[ -n "$expected" ]] || die "no sha256 available for ${tarball}: pin one in lib/common.sh, pass --sha256, or check connectivity to www.kernel.org. Refusing to build an unverified guest kernel."

  local actual
  actual="$(sha256sum "$tarball" | awk '{print $1}')"
  [[ "$actual" == "$expected" ]] \
    || die "sha256 mismatch for ${tarball}: expected ${expected}, got ${actual}. The download is corrupt or tampered with; delete it and retry."
  ok "sha256 verified: ${tarball}"
}

if [[ ! -d "$src_dir" ]]; then
  log "downloading ${tarball}"
  curl -sfL -o "$tarball" \
    "https://cdn.kernel.org/pub/linux/kernel/v${major}.x/${tarball}" \
    || die "failed to download kernel ${KERNEL_VERSION}. Check the version exists on cdn.kernel.org."
  verify_kernel_tarball
  log "extracting"
  tar -xf "$tarball"
fi
cd "$src_dir"
ok "source at ${BUILD_DIR}/${src_dir}"

section "3. Configuration"
if [[ -n "$CUSTOM_CONFIG" ]]; then
  [[ -f "$CUSTOM_CONFIG" ]] || die "config not found: ${CUSTOM_CONFIG}"
  cp "$CUSTOM_CONFIG" .config
  ok "using custom config ${CUSTOM_CONFIG}"
else
  # Use the Firecracker project's CI guest config as the baseline. It is the
  # configuration upstream validates against, so it has the virtio drivers a
  # microVM guest needs and little else.
  #
  # Fetched from an immutable commit (FIRECRACKER_GUEST_CONFIG_REF in
  # lib/common.sh), not a branch: what "main" serves can change between two
  # builds, silently altering the kernel configuration inside every microVM.
  # Where a checksum is pinned for this arch/version, the download is verified
  # against it as well.
  arch="$(normalize_arch)"
  config_name="microvm-kernel-ci-${arch}-${KERNEL_VERSION}.config"
  config_url="https://raw.githubusercontent.com/${FIRECRACKER_REPO}/${FIRECRACKER_GUEST_CONFIG_REF}/resources/guest_configs/${config_name}"

  log "fetching ${config_name} @ ${FIRECRACKER_GUEST_CONFIG_REF:0:12}"
  curl -sfL -o .config "$config_url" \
    || die "no upstream config for ${arch} v${KERNEL_VERSION} at the pinned ref. Pass --config to supply one, or update FIRECRACKER_GUEST_CONFIG_REF in lib/common.sh."

  expected_cfg="$(guest_config_sha256 "${arch}-${KERNEL_VERSION}")"
  if [[ -n "$expected_cfg" ]]; then
    actual_cfg="$(sha256sum .config | awk '{print $1}')"
    [[ "$actual_cfg" == "$expected_cfg" ]] \
      || die "sha256 mismatch for ${config_name}: expected ${expected_cfg}, got ${actual_cfg}. Immutable refs do not change content; investigate before building."
    ok "using upstream guest config (pinned ref, sha256 verified)"
  else
    warn "no pinned sha256 for ${config_name}; using the pinned ref without content verification"
    ok "using upstream guest config (pinned ref)"
  fi
fi

# Ensure squashfs is available. A shared read-only squashfs rootfs means one host
# page-cache copy serves every microVM instead of one per VM, which matters at density.
section "4. Config adjustments"
enable_opt() {
  local opt="$1"
  if grep -q "^${opt}=y" .config; then
    ok "  ${opt} already enabled"
  else
    sed -i "/^# ${opt} is not set/d;/^${opt}=/d" .config
    printf '%s=y\n' "$opt" >> .config
    ok "  ${opt} enabled"
  fi
}

for opt in CONFIG_SQUASHFS CONFIG_SQUASHFS_ZSTD CONFIG_SQUASHFS_XZ CONFIG_OVERLAY_FS; do
  enable_opt "$opt"
done

# Resolve any new symbol dependencies non-interactively, taking defaults.
make olddefconfig >/dev/null
ok "config finalized"

section "5. Build"
jobs="$(nproc)"
log "building vmlinux with ${jobs} parallel jobs (this takes a while)"
if [[ "$(uname -m)" == "x86_64" ]]; then
  make -j"${jobs}" vmlinux >/dev/null
  artifact="vmlinux"
else
  make -j"${jobs}" Image >/dev/null
  artifact="arch/arm64/boot/Image"
fi
[[ -f "$artifact" ]] || die "build produced no ${artifact}"
ok "built ${artifact} ($(du -h "$artifact" | cut -f1))"

section "6. Install"
mkdir -p "$ARTIFACT_DIR"
install -m 0644 "$artifact" "${ARTIFACT_DIR}/vmlinux"
install -m 0644 .config "${ARTIFACT_DIR}/vmlinux-${KERNEL_VERSION}.config"
ok "installed ${ARTIFACT_DIR}/vmlinux"
ok "config saved to ${ARTIFACT_DIR}/vmlinux-${KERNEL_VERSION}.config"

cat <<SUMMARY

  Guest kernel:  v${KERNEL_VERSION}
  Artifact:      ${ARTIFACT_DIR}/vmlinux
  Config:        ${ARTIFACT_DIR}/vmlinux-${KERNEL_VERSION}.config
  Build tree:    ${BUILD_DIR}/${src_dir}  (safe to delete)

  Guest boot arguments to use with this kernel:
    console=  8250.nr_uarts=0  reboot=k  panic=1  pci=off

  8250.nr_uarts=0 disables the serial device. A guest-visible serial console is
  tied to VMM stdout and can cause unbounded host memory or storage use. Note the
  guest can re-enable it, so also bound VMM stdout.

SUMMARY

ok "done"
