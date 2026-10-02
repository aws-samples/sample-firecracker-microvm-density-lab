#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Toggle SMT on a running bare metal host and verify the change took effect.
#
# On EC2 bare metal this is the only way to change SMT state. CpuOptions, which sets
# ThreadsPerCore at launch on virtualized instances, is rejected on metal:
#
#   UnsupportedOperation: The m8i.metal-48xl instance type does not support
#   specifying CpuOptions.
#
# describe-instance-types shows why: ValidThreadsPerCore is null for metal SKUs and [1,2] for
# the virtualized m8i.48xlarge. CpuOptions is a Nitro hypervisor feature, and metal has no
# hypervisor to enforce it.
#
# This is an advantage rather than a workaround. Measuring both SMT states on one physical
# machine removes the between-host confound that two differently-launched instances would carry:
# a difference cannot be attributed to "this particular host".
#
# One caveat to state when reporting results: offlining siblings at runtime is not identical to
# never having booted with them. The kernel rebuilds scheduler domains, but memory was allocated
# and interrupts were routed while all CPUs were present. The distinction is worth noting; it
# does not invalidate the comparison.
#
# Usage:
#   sudo ./toggle-smt.sh off      # offline every SMT sibling
#   sudo ./toggle-smt.sh on       # bring siblings back online
#   ./toggle-smt.sh status        # report current state, no root needed
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

SMT_CONTROL=/sys/devices/system/cpu/smt/control
SMT_ACTIVE=/sys/devices/system/cpu/smt/active

ACTION="${1:-status}"

# Count logical CPUs, physical cores, and threads per core from sysfs rather than lscpu, so the
# check does not depend on a package that may not be installed.
#
# topology/core_cpus_list (or thread_siblings_list on older kernels) lists the siblings sharing a
# physical core. Deduplicating those lists gives the physical core count.
report_topology() {
  local online cores threads
  online="$(grep -c '^processor' /proc/cpuinfo)"

  # Deduplicating the sibling lists of online CPUs counts physical cores. Offline CPUs are
  # skipped because their sibling list still names them, which would keep the core count
  # unchanged after an SMT-off toggle and make threads-per-core look wrong.
  cores="$(
    for cpu in /sys/devices/system/cpu/cpu[0-9]*; do
      if [[ -e "${cpu}/online" ]] && [[ "$(cat "${cpu}/online" 2>/dev/null || echo 1)" == "0" ]]; then
        continue
      fi
      local sib_file="${cpu}/topology/core_cpus_list"
      [[ -r "$sib_file" ]] || sib_file="${cpu}/topology/thread_siblings_list"
      [[ -r "$sib_file" ]] || continue
      cat "$sib_file"
    done | sort -u | wc -l | tr -d ' '
  )"

  if [[ "${cores:-0}" -gt 0 ]]; then
    threads=$(( online / cores ))
  else
    threads=0
  fi

  printf '  online logical CPUs: %s\n' "$online"
  printf '  physical cores:      %s\n' "$cores"
  printf '  threads per core:    %s\n' "$threads"
  printf '  smt/control:         %s\n' "$(cat "$SMT_CONTROL" 2>/dev/null || echo '<absent>')"
  printf '  smt/active:          %s\n' "$(cat "$SMT_ACTIVE" 2>/dev/null || echo '<absent>')"
}

[[ -r "$SMT_CONTROL" ]] || die "$SMT_CONTROL is absent; this kernel does not expose SMT control"

CURRENT="$(cat "$SMT_CONTROL")"

case "$ACTION" in
  status)
    section "SMT status"
    report_topology
    exit 0
    ;;
  on|off) ;;
  *) die "usage: $0 [on|off|status]" ;;
esac

# notsupported means the hardware has one thread per core, which is the expected state on AMD
# metal. Treat it as a positive confirmation rather than an error, but refuse the toggle.
if [[ "$CURRENT" == "notsupported" ]]; then
  warn "this host reports smt/control=notsupported: the hardware has one thread per core"
  warn "there is no SMT to toggle. Configs C, E, and F will be skipped with a recorded reason."
  section "SMT status"
  report_topology
  exit 0
fi

# forceoff means SMT was disabled by a boot parameter (nosmt) and cannot be re-enabled without a
# reboot. Fail loudly: a sweep labelled smt-on would otherwise silently measure smt-off.
if [[ "$CURRENT" == "forceoff" && "$ACTION" == "on" ]]; then
  die "smt/control=forceoff: SMT was disabled at boot and cannot be re-enabled at runtime. Reboot without the nosmt kernel parameter."
fi

require_root

section "Toggling SMT ${CURRENT} -> ${ACTION}"

if [[ "$CURRENT" == "$ACTION" ]]; then
  ok "already $ACTION; nothing to do"
  report_topology
  exit 0
fi

# Offlining ~96 CPUs takes a moment and the write blocks until the kernel finishes, so this is
# synchronous rather than fire-and-forget.
log "writing $ACTION to $SMT_CONTROL"
echo "$ACTION" > "$SMT_CONTROL"

# Read back rather than trusting the write. The kernel can accept a write and refuse the
# transition, and a sweep run against an unchanged host would measure the wrong configuration
# while reporting the intended label.
NEW="$(cat "$SMT_CONTROL")"
[[ "$NEW" == "$ACTION" ]] || die "wrote $ACTION but smt/control now reads $NEW; the transition was refused"

# smt/active is the authoritative signal that siblings are actually offline: 1 means SMT is in
# use, 0 means it is not.
ACTIVE="$(cat "$SMT_ACTIVE" 2>/dev/null || echo unknown)"
case "$ACTION" in
  off) [[ "$ACTIVE" == "0" ]] || die "smt/control=off but smt/active=$ACTIVE; siblings are still online" ;;
  on)  [[ "$ACTIVE" == "1" ]] || die "smt/control=on but smt/active=$ACTIVE; siblings did not come back online" ;;
esac

ok "SMT is now $ACTION (smt/active=$ACTIVE)"
report_topology

cat <<NEXT

  Record the state with the sweep so the dataset is self-describing:

    labsweep -run-label smt-${ACTION} -requested-threads-per-core $(
      [[ "$ACTION" == "off" ]] && echo 1 || echo 2
    ) ...

  labsweep compares the requested thread count against what it observes and reports a
  mismatch as a leading caveat, so a toggle that silently failed cannot be mistaken for a
  valid result.

NEXT
