#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Provision an EC2 bare metal host for dense Firecracker microVM workloads.
#
# Run this once on a .metal instance, then snapshot the result as an AMI. Settings
# that require a reboot are written here so they are already in effect when an
# instance launches from the AMI, rather than costing a reboot cycle during
# autoscaling.
#
# What this script does NOT do, by design:
#   - NUMA topology discovery and cpuset allocation. Real topology is not knowable
#     until the process runs on the target hardware, so the host agent does this at
#     runtime.
#   - SMT configuration. Runtime-togglable via sysfs, so it belongs to the agent.
#   - Warm pool prefill. Depends on instance core count and configured pool size.
#
# Usage:
#   sudo ./provision-host.sh [--skip-reboot] [--dry-run]
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

SKIP_REBOOT=0
DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --skip-reboot) SKIP_REBOOT=1 ;;
    --dry-run)     DRY_RUN=1 ;;
    -h|--help)     sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)             die "unknown argument: $arg" ;;
  esac
done

run() {
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '  would run: %s\n' "$*"
  else
    "$@"
  fi
}

write_file() {
  local path="$1"; shift
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '  would write %s:\n' "$path"
    printf '    | %s\n' "$@"
  else
    mkdir -p "$(dirname "$path")"
    printf '%s\n' "$@" > "$path"
  fi
}

# A dry run changes nothing, so it does not need root and does not need to be on
# a metal instance. This makes the script reviewable from a workstation.
if [[ $DRY_RUN -eq 0 ]]; then
  require_root
  assert_bare_metal
else
  log "dry run: no changes will be made, root not required"
fi

# On a dry run the local machine may not be the target distro, so tolerate a
# missing package manager rather than aborting.
if ! PKG_MGR="$(detect_pkg_mgr)"; then
  if [[ $DRY_RUN -eq 1 ]]; then
    PKG_MGR=dnf
    warn "no package manager found locally; assuming ${PKG_MGR} for dry run output"
  else
    die "no supported package manager found (need dnf or apt-get)"
  fi
fi

# ---------------------------------------------------------------------------
section "1. Base packages"
# ---------------------------------------------------------------------------
# numactl and util-linux provide the topology tools the host agent shells out to.
# The rest support image building and measurement.
#
# curl is deliberately absent from this list. Amazon Linux 2023 ships curl-minimal,
# which already provides /usr/bin/curl; asking for the curl package forces a
# conflicting package swap that dnf refuses. Presence is checked below instead.
PACKAGES_COMMON=(tar numactl util-linux e2fsprogs squashfs-tools iproute)

# nftables is required, not optional. Minimal AL2023 images ship with no firewall
# tooling at all, and without it the IMDS drop rule in section 9 cannot be applied,
# leaving guests able to reach the host's instance metadata service.
case "$PKG_MGR" in
  dnf) PACKAGES_COMMON+=(perf nftables) ;;
  apt) PACKAGES_COMMON+=(linux-tools-generic nftables) ;;
esac

# curl is required but must not be installed by name. Every supported distro ships
# it (as curl or curl-minimal), so treat absence as a hard error rather than
# attempting a swap.
if [[ $DRY_RUN -eq 0 ]]; then
  command -v curl >/dev/null 2>&1 || die "curl not found and cannot be installed safely; install curl or curl-minimal first"
  ok "curl present: $(curl --version | head -1 | cut -d' ' -f1-2)"
fi

# Install only what is missing. Re-running with everything present should be a
# no-op rather than a package transaction, which keeps the script idempotent and
# avoids surprise upgrades of unrelated packages on an already-built image.
missing=()
if [[ $DRY_RUN -eq 0 ]]; then
  for p in "${PACKAGES_COMMON[@]}"; do
    case "$PKG_MGR" in
      dnf) rpm -q --whatprovides "$p" >/dev/null 2>&1 || missing+=("$p") ;;
      apt) dpkg -s "$p" >/dev/null 2>&1 || missing+=("$p") ;;
    esac
  done
else
  missing=("${PACKAGES_COMMON[@]}")
fi

if (( ${#missing[@]} == 0 )); then
  ok "all base packages already present"
else
  log "installing ${#missing[@]} missing package(s): ${missing[*]}"
  case "$PKG_MGR" in
    dnf) run dnf install -y "${missing[@]}" ;;
    apt) run env DEBIAN_FRONTEND=noninteractive apt-get update -qq
         run env DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}" ;;
  esac
  ok "base packages installed"
fi

# ---------------------------------------------------------------------------
section "2. KVM availability"
# ---------------------------------------------------------------------------
if [[ $DRY_RUN -eq 0 ]]; then
  [[ -e /dev/kvm ]] || die "/dev/kvm not present. Confirm this is a .metal instance."
  [[ -r /dev/kvm && -w /dev/kvm ]] || warn "/dev/kvm exists but is not read-write for this user"
  ok "/dev/kvm present"

  vendor="$(cpu_vendor)"
  case "$vendor" in
    GenuineIntel) kvm_mod=kvm_intel ;;
    AuthenticAMD) kvm_mod=kvm_amd ;;
    *)            die "unrecognized CPU vendor: ${vendor}" ;;
  esac
  ok "CPU vendor ${vendor}, KVM module ${kvm_mod}"
else
  kvm_mod=kvm_intel
fi

# ---------------------------------------------------------------------------
section "3. Firecracker and jailer"
# ---------------------------------------------------------------------------
# Pinned version. Snapshots are only resumable on the same Firecracker version,
# so bumping this invalidates existing snapshots fleet-wide.
arch="$(normalize_arch)"
tarball="firecracker-${FIRECRACKER_VERSION}-${arch}.tgz"
base_url="https://github.com/${FIRECRACKER_REPO}/releases/download/${FIRECRACKER_VERSION}"

log "downloading Firecracker ${FIRECRACKER_VERSION} (${arch})"
if [[ $DRY_RUN -eq 0 ]]; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' EXIT

  curl -sfL -o "${tmp}/${tarball}" "${base_url}/${tarball}" \
    || die "failed to download ${tarball}"
  curl -sfL -o "${tmp}/${tarball}.sha256.txt" "${base_url}/${tarball}.sha256.txt" \
    || die "failed to download checksum for ${tarball}"

  # Verify before unpacking. The checksum file names the artifact, so run the
  # check from the directory holding both.
  ( cd "${tmp}" && sha256sum -c "${tarball}.sha256.txt" >/dev/null ) \
    || die "checksum verification failed for ${tarball}"
  ok "checksum verified"

  tar -xzf "${tmp}/${tarball}" -C "${tmp}"
  release_dir="${tmp}/release-${FIRECRACKER_VERSION}-${arch}"

  install -m 0755 "${release_dir}/firecracker-${FIRECRACKER_VERSION}-${arch}" \
    "${INSTALL_DIR}/firecracker"
  install -m 0755 "${release_dir}/jailer-${FIRECRACKER_VERSION}-${arch}" \
    "${INSTALL_DIR}/jailer"

  ok "installed $("${INSTALL_DIR}/firecracker" --version | head -1)"
else
  printf '  would download and verify %s\n' "${base_url}/${tarball}"
fi

run mkdir -p "${ARTIFACT_DIR}" "${JAILER_CHROOT_BASE}"
# The jailer treats all its inputs as trusted, so the chroot base and its parents
# must not be writable by unprivileged users.
run chown root:root "${JAILER_CHROOT_BASE}"
run chmod 0755 "${JAILER_CHROOT_BASE}"
ok "jailer chroot base at ${JAILER_CHROOT_BASE}"

# ---------------------------------------------------------------------------
section "4. Guest kernel"
# ---------------------------------------------------------------------------
# Uncompressed vmlinux is recommended over bzImage: a bzImage is decompressed by
# the guest at boot, costing boot time and guest memory, which multiplies at density.
log "guest kernel target: v${GUEST_KERNEL_VERSION}"
if [[ "${GUEST_KERNEL_VERSION}" == "6.1" ]]; then
  warn "guest kernel v6.1 support ends 2026-09-02; prefer v6.18 for new work"
fi
warn "kernel image is not built here. Run build-guest-kernel.sh to produce ${ARTIFACT_DIR}/vmlinux"

# ---------------------------------------------------------------------------
section "5. Host kernel command line"
# ---------------------------------------------------------------------------
# These require a reboot, which is exactly why they are baked into the AMI.
#
#   quiet loglevel=1     Console logging degrades operations measurably. Upstream
#                        saw snapshot restore go from 3ms to 8.5ms with console
#                        logging enabled.
#   transparent_hugepage Left at default; hugepage strategy is decided per workload.
KERNEL_ARGS="quiet loglevel=1"

log "setting kernel args: ${KERNEL_ARGS}"
if [[ $DRY_RUN -eq 0 ]]; then
  if [[ -f /etc/default/grub ]]; then
    if grep -q "^GRUB_CMDLINE_LINUX_DEFAULT" /etc/default/grub; then
      sed -i "s|^GRUB_CMDLINE_LINUX_DEFAULT=.*|GRUB_CMDLINE_LINUX_DEFAULT=\"${KERNEL_ARGS}\"|" \
        /etc/default/grub
    else
      printf 'GRUB_CMDLINE_LINUX_DEFAULT="%s"\n' "${KERNEL_ARGS}" >> /etc/default/grub
    fi

    if command -v grub2-mkconfig >/dev/null 2>&1; then
      grub2-mkconfig -o /boot/grub2/grub.cfg >/dev/null 2>&1 || \
        warn "grub2-mkconfig failed; kernel args may not persist"
    elif command -v update-grub >/dev/null 2>&1; then
      update-grub >/dev/null 2>&1 || warn "update-grub failed; kernel args may not persist"
    else
      warn "no grub config tool found; set kernel args manually"
    fi
    ok "kernel command line updated (takes effect after reboot)"
  else
    warn "/etc/default/grub not found; set kernel args manually for this distro"
  fi
fi

# ---------------------------------------------------------------------------
section "6. KVM module parameters"
# ---------------------------------------------------------------------------
# Linux 6.1 regressed KVM_CREATE_VM latency, which matters when creating microVMs
# at a high rate. If the host is not vulnerable to iTLB multihit, disabling
# nx_huge_pages is the least invasive fix because it is scoped to the KVM module
# rather than changing cgroup behaviour host-wide.
itlb=/sys/devices/system/cpu/vulnerabilities/itlb_multihit
itlb_status="$( [[ -r "$itlb" ]] && cat "$itlb" || echo "unknown" )"
log "itlb_multihit: ${itlb_status}"

# "Not affected" is the only case where disabling nx_huge_pages is clearly safe.
# On anything else, including an unreadable file, leave the mitigation in place:
# a slower microVM creation rate is preferable to removing a CPU mitigation on a
# host whose exposure is unverified.
if [[ "${itlb_status}" == "Not affected" ]]; then
  write_file /etc/modprobe.d/kvm-firecracker.conf \
    "# Mitigates the Linux 6.1 KVM_CREATE_VM latency regression." \
    "# Safe here because this host reports itlb_multihit: Not affected." \
    "options kvm nx_huge_pages=never"
  ok "set kvm nx_huge_pages=never"
elif [[ "${itlb_status}" == "unknown" ]]; then
  warn "could not read ${itlb}; leaving nx_huge_pages at default"
else
  warn "host reports itlb_multihit: ${itlb_status}; leaving nx_huge_pages at default"
  warn "to recover microVM creation rate, mount cgroup2 with favordynmods instead:"
  warn "  mount -o remount,favordynmods /sys/fs/cgroup"
fi

# Bound timer interrupt injection rate. Each guest injecting timer interrupts
# spawns a kvm-pit kthread in the root cgroup; the host agent moves those into the
# per-microVM cgroup after guest start, since Firecracker is de-privileged by then.
write_file /etc/modprobe.d/kvm-timer.conf \
  "# Lower bound on timer period reduces kvm-pit kthread CPU overhead at density." \
  "# Validate against your workload: this also applies to APIC timer events." \
  "options kvm min_timer_period_us=200"
ok "set kvm min_timer_period_us=200"

# ---------------------------------------------------------------------------
section "7. Memory hygiene"
# ---------------------------------------------------------------------------
# Swap off: prevents guest memory contents from being written to host storage.
log "disabling swap"
if [[ $DRY_RUN -eq 0 ]]; then
  swapoff -a || true
  # Comment out swap entries so they do not return after reboot.
  if [[ -f /etc/fstab ]] && grep -qE '^[^#].*\sswap\s' /etc/fstab; then
    sed -i.bak -E 's|^([^#].*\sswap\s.*)$|# disabled for Firecracker: \1|' /etc/fstab
    ok "swap entries commented out in /etc/fstab"
  fi
  if grep -q '/dev' /proc/swaps 2>/dev/null; then
    warn "swap still present in /proc/swaps"
  else
    ok "no active swap"
  fi
fi

# KSM off: page deduplication across VMs is a documented side-channel risk, and it
# would also work against the CoW page-cache sharing the warm pool relies on.
#
# Amazon Linux 2023 does not compile KSM in at all (CONFIG_KSM is not set), so the
# sysfs path is absent. That is the desired end state, not a problem: KSM cannot be
# switched on. Distinguish this from "present but enabled", which does need action.
log "checking KSM"
if [[ $DRY_RUN -eq 1 ]]; then
  printf '  would disable KSM if present\n'
elif [[ ! -d /sys/kernel/mm/ksm ]]; then
  ok "KSM not compiled into this kernel (CONFIG_KSM unset). Nothing to disable."
elif [[ -w /sys/kernel/mm/ksm/run ]]; then
  echo 0 > /sys/kernel/mm/ksm/run
  write_file /etc/systemd/system/disable-ksm.service \
    "[Unit]" \
    "Description=Disable Kernel Samepage Merging for Firecracker hosts" \
    "After=multi-user.target" \
    "" \
    "[Service]" \
    "Type=oneshot" \
    "ExecStart=/bin/sh -c 'echo 0 > /sys/kernel/mm/ksm/run'" \
    "RemainAfterExit=yes" \
    "" \
    "[Install]" \
    "WantedBy=multi-user.target"
  run systemctl enable disable-ksm.service
  ok "KSM disabled and persisted across reboot"
else
  warn "/sys/kernel/mm/ksm exists but run is not writable; check KSM state manually"
fi

# ---------------------------------------------------------------------------
section "8. cgroup v2"
# ---------------------------------------------------------------------------
# cgroup v2 is required: v1 has known high snapshot restore latency, and the
# cpuset partition interface the placement agent depends on is v2-only.
if [[ $DRY_RUN -eq 0 ]]; then
  if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
    ok "cgroup v2 (unified hierarchy) active"
    for c in cpuset cpu memory io; do
      grep -qw "$c" /sys/fs/cgroup/cgroup.controllers \
        && ok "  controller available: $c" \
        || warn "  controller missing: $c"
    done
  else
    die "cgroup v2 not active. Add systemd.unified_cgroup_hierarchy=1 to the kernel command line."
  fi
fi

# ---------------------------------------------------------------------------
section "9. Guest network egress filtering"
# ---------------------------------------------------------------------------
# Firecracker does no traffic filtering: packets go straight from the guest NIC to
# the tap device. Blocking IMDS from guests is the baseline rule, otherwise guest
# code can reach the host's instance credentials.
log "installing IMDS drop rule for tap devices"
if [[ $DRY_RUN -eq 1 ]]; then
  printf '  would add nft rule dropping tap* traffic to 169.254.169.254\n'
  printf '  would persist it via /etc/sysconfig/nftables.conf and enable nftables.service\n'
elif command -v nft >/dev/null 2>&1; then
  nft list table inet firecracker >/dev/null 2>&1 || nft add table inet firecracker
  nft list chain inet firecracker filter >/dev/null 2>&1 || \
    nft add chain inet firecracker filter '{ type filter hook forward priority 0; }'
  # Idempotent: only add if an equivalent rule is not already present.
  if nft -a list chain inet firecracker filter 2>/dev/null | grep -q '169.254.169.254'; then
    ok "nft IMDS drop rule already present"
  else
    nft add rule inet firecracker filter iifname "tap*" ip daddr 169.254.169.254 counter drop
    ok "nft IMDS drop rule installed"
  fi

  # Persist across reboot. Without this the rule is lost and guests silently regain
  # IMDS access after the AMI is launched, which is worse than never having it.
  ruleset_file=/etc/firecracker-nftables.conf
  if [[ -d /etc/sysconfig ]]; then
    write_file "$ruleset_file" \
      "#!/usr/sbin/nft -f" \
      "# Drop guest traffic to the instance metadata service." \
      "add table inet firecracker" \
      "add chain inet firecracker filter { type filter hook forward priority 0; }" \
      "add rule inet firecracker filter iifname \"tap*\" ip daddr 169.254.169.254 counter drop"

    # nftables.service reads includes from /etc/sysconfig/nftables.conf.
    if [[ -f /etc/sysconfig/nftables.conf ]] && \
       ! grep -q "$ruleset_file" /etc/sysconfig/nftables.conf; then
      printf 'include "%s"\n' "$ruleset_file" >> /etc/sysconfig/nftables.conf
    fi
    systemctl enable nftables.service >/dev/null 2>&1 && \
      ok "rule persisted via nftables.service" || \
      warn "could not enable nftables.service; rule will not survive reboot"
  else
    warn "no /etc/sysconfig; persist the nft ruleset manually for this distro"
  fi
else
  # Reaching here means the package install did not provide nftables. Treat it as
  # fatal: continuing would produce an AMI whose guests can read instance metadata.
  die "nft not found. The IMDS drop rule cannot be applied, so guests could reach instance metadata. Install nftables and re-run."
fi

# ---------------------------------------------------------------------------
section "10. File descriptor limits"
# ---------------------------------------------------------------------------
# At 150 to 180 microVMs per host, each with a VMM process holding sockets and
# memory-mapped files, the default limits are too low. The jailer defaults no-file
# to 2048 per process, which is separate from these host-wide limits.
# limits.d is enforced by PAM, so it covers interactive logins only.
write_file /etc/security/limits.d/99-firecracker.conf \
  "# Raised for high microVM density (150 to 180 VMMs per host)." \
  "# Applies to PAM sessions (interactive logins) only. Services are covered by" \
  "# the systemd drop-ins below." \
  "root soft nofile 1048576" \
  "root hard nofile 1048576" \
  "*    soft nofile 1048576" \
  "*    hard nofile 1048576"

# Systemd services do NOT inherit limits.d. Amazon Linux 2023 ships
# DefaultLimitNOFILE=65535, which would cap the host agent well below what 150 or
# more microVMs need (each contributes an API socket, memory-mapped file, tap device,
# and vsock). Raise the systemd defaults too, or the limit above is cosmetic.
write_file /etc/systemd/system.conf.d/99-firecracker.conf \
  "# Raised for high microVM density. limits.d does not apply to services." \
  "[Manager]" \
  "DefaultLimitNOFILE=1048576:1048576" \
  "DefaultLimitNPROC=1048576:1048576"

write_file /etc/systemd/user.conf.d/99-firecracker.conf \
  "# Match the system manager limits for user services." \
  "[Manager]" \
  "DefaultLimitNOFILE=1048576:1048576"

write_file /etc/sysctl.d/99-firecracker.conf \
  "# Raised for high microVM density." \
  "fs.file-max = 2097152" \
  "# Each microVM adds tap devices, sockets, and processes." \
  "kernel.pid_max = 4194304" \
  "net.core.somaxconn = 4096"

# Apply only our own file. `sysctl --system` reloads every file on the host, which on
# Amazon Linux 2023 emits errors for pre-existing keys unrelated to this script
# (net.ipv4.conf.all.rp_filter and similar) and makes real failures hard to spot.
if [[ $DRY_RUN -eq 0 ]]; then
  sysctl -p /etc/sysctl.d/99-firecracker.conf >/dev/null
fi
ok "limits and sysctls written"

# ---------------------------------------------------------------------------
section "Summary"
# ---------------------------------------------------------------------------
cat <<SUMMARY

  Firecracker:      ${FIRECRACKER_VERSION}
  Guest kernel:     v${GUEST_KERNEL_VERSION} (build separately)
  Artifacts:        ${ARTIFACT_DIR}
  Jailer chroot:    ${JAILER_CHROOT_BASE}
  CPU vendor:       $( [[ $DRY_RUN -eq 0 ]] && cpu_vendor || echo "n/a (dry run)" )
  SMT state:        $( [[ $DRY_RUN -eq 0 ]] && smt_state || echo "n/a (dry run)" )

  Next steps:
    1. ./build-guest-kernel.sh          build ${ARTIFACT_DIR}/vmlinux
    2. reboot                           apply kernel args and module params
    3. ./validate-host.sh               confirm the host is ready
    4. snapshot this instance as an AMI

  Deliberately left to the host agent at runtime:
    NUMA topology discovery, cpuset allocation and partition setup,
    SMT configuration, kvm-pit thread placement, warm pool prefill.

SUMMARY

if [[ $SKIP_REBOOT -eq 0 && $DRY_RUN -eq 0 ]]; then
  warn "reboot required for kernel args and KVM module parameters"
  printf 'Reboot now? [y/N] '
  read -r reply
  [[ "$reply" =~ ^[Yy]$ ]] && { log "rebooting"; reboot; }
fi

ok "provisioning complete"
