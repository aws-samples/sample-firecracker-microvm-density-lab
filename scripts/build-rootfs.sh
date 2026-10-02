#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Build a minimal ext4 root filesystem for Firecracker microVMs.
#
# The image is deliberately small. It is shared read-only by every microVM on the host, so
# one copy sits in the page cache and serves the whole fleet rather than one copy per VM.
# That sharing is what makes an idle warm pool cheap, and every megabyte added here is a
# megabyte of page cache not available for anything else.
#
# Contents: busybox userland, the guest agent as PID 1, and nothing else. There is no
# package manager, no shell login, and no service manager, because a microVM that runs one
# task and is then destroyed needs none of them.
#
# Usage:
#   sudo ./build-rootfs.sh --agent /path/to/guest-agent [--size 64] [--out rootfs.ext4]
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"
# shellcheck source=lib/rootfs.sh
source "${SCRIPT_DIR}/lib/rootfs.sh"

AGENT_BIN=""
SIZE_MIB=64
OUT=""
BUSYBOX_URL="${BUSYBOX_URL:-https://busybox.net/downloads/binaries/1.35.0-x86_64-linux-musl/busybox}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --agent) AGENT_BIN="$2"; shift 2 ;;
    --size)  SIZE_MIB="$2"; shift 2 ;;
    --out)   OUT="$2"; shift 2 ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

OUT="${OUT:-${ARTIFACT_DIR}/rootfs.ext4}"

# Validate arguments before checking privileges, so a missing or wrong --agent is reported
# without the user first having to work out that sudo was needed.
[[ -n "$AGENT_BIN" ]] || die "--agent is required: the guest agent runs as PID 1"
[[ -f "$AGENT_BIN" ]] || die "agent binary not found: $AGENT_BIN"
[[ "$SIZE_MIB" =~ ^[0-9]+$ ]] || die "--size must be an integer number of MiB, got $SIZE_MIB"
(( SIZE_MIB >= 8 )) || die "--size ${SIZE_MIB} MiB is too small for a formatted ext4 image"

require_root
require_cmd mkfs.ext4
require_cmd curl

# A dynamically linked agent would need its interpreter and libraries copied in, which is
# both easy to get wrong and larger. Static linking is required, not merely preferred.
if command -v file >/dev/null 2>&1; then
  if file "$AGENT_BIN" | grep -q "dynamically linked"; then
    die "agent must be statically linked. Build with a musl target, for example: cargo build --release --target x86_64-unknown-linux-musl"
  fi
  ok "agent is statically linked"
fi

section "1. Create and format the image"
mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"

# A sparse file: the image reports its full size but occupies only what is written. At 180
# microVMs this matters if per-VM overlays are ever created from it.
log "creating ${SIZE_MIB} MiB sparse image at ${OUT}"
dd if=/dev/zero of="$OUT" bs=1M count=0 seek="$SIZE_MIB" status=none

# Disable the journal: the image is mounted read-only by guests, so a journal is dead weight
# in both image size and page cache.
log "formatting ext4 without a journal"
mkfs.ext4 -q -O ^has_journal -L fcrootfs "$OUT"
ok "image formatted"

section "2. Populate"
MNT="$(mktemp -d)"
cleanup() {
  if mountpoint -q "$MNT" 2>/dev/null; then
    umount "$MNT" || true
  fi
  rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

mount -o loop "$OUT" "$MNT"

# Only the directories a booting kernel and the agent actually need.
mkdir -p "$MNT"/{bin,dev,etc,proc,sys,tmp,run,work}
chmod 1777 "$MNT/tmp"

log "installing busybox"
if [[ -n "${BUSYBOX_BIN:-}" && -f "${BUSYBOX_BIN}" ]]; then
  install -m 0755 "${BUSYBOX_BIN}" "$MNT/bin/busybox"
  ok "used local busybox from BUSYBOX_BIN"
elif command -v busybox >/dev/null 2>&1 && ! file "$(command -v busybox)" 2>/dev/null | grep -q "dynamically linked"; then
  install -m 0755 "$(command -v busybox)" "$MNT/bin/busybox"
  ok "used host busybox (static)"
else
  log "downloading static busybox"
  curl -sfL -o "$MNT/bin/busybox" "$BUSYBOX_URL" \
    || die "failed to download busybox from ${BUSYBOX_URL}. Set BUSYBOX_BIN to a local static binary instead."
  chmod 0755 "$MNT/bin/busybox"
  ok "downloaded static busybox"
fi

# Symlink the handful of applets that are genuinely useful for debugging a guest. Installing
# the full applet set would add hundreds of links for no benefit here.
for applet in sh ls cat mount umount ps kill sleep dmesg mkdir echo; do
  ln -sf busybox "$MNT/bin/$applet"
done

log "installing guest agent as /init"
install -m 0755 "$AGENT_BIN" "$MNT/init"

# Minimal identity files. Some libc paths consult these even in a static binary.
printf 'root:x:0:0:root:/:/bin/sh\n' > "$MNT/etc/passwd"
printf 'root:x:0:\n' > "$MNT/etc/group"
printf 'fc-guest\n' > "$MNT/etc/hostname"

# The guest kernel mounts these itself when the agent asks, but the mount points must exist.
cat > "$MNT/etc/fstab" <<'FSTAB'
proc  /proc  proc  defaults  0 0
sysfs /sys   sysfs defaults  0 0
FSTAB

# /dev is created above but left empty, and nothing mounts devtmpfs in the guest. The synthetic
# workloads have not needed a device node, so this was latent rather than broken -- but any busybox
# applet that redirects to /dev/null fails without it, which is a confusing way to lose a debugging
# session inside a guest.
rootfs_make_devices "$MNT"

section "3. Verify"
ls -la "$MNT/init" >/dev/null || die "agent was not installed"
[[ -x "$MNT/init" ]] || die "/init is not executable; the kernel would panic on boot"
[[ -x "$MNT/bin/busybox" ]] || die "busybox is not executable"

USED_KIB="$(du -sk "$MNT" | cut -f1)"
umount "$MNT"

# fsck confirms the image is structurally sound. A corrupt image fails at guest boot with an
# opaque kernel panic rather than anything pointing back to the build.
if command -v e2fsck >/dev/null 2>&1; then
  e2fsck -fp "$OUT" >/dev/null 2>&1 || warn "e2fsck reported issues; inspect ${OUT}"
  ok "filesystem check passed"
fi

APPARENT_MIB=$(( $(stat -c %s "$OUT") / 1024 / 1024 ))
ACTUAL_KIB="$(du -k "$OUT" | cut -f1)"

cat <<SUMMARY

  Image:          ${OUT}
  Apparent size:  ${APPARENT_MIB} MiB
  On-disk size:   $(( ACTUAL_KIB / 1024 )) MiB (sparse)
  Content used:   $(( USED_KIB / 1024 )) MiB
  Init:           /init (the guest agent)

  Attach it read-only, so one page-cache copy serves every microVM:

    {"drive_id":"rootfs","path_on_host":"rootfs.ext4",
     "is_root_device":true,"is_read_only":true}

  Guest boot arguments to pair with it:

    ro console= 8250.nr_uarts=0 reboot=k panic=1 pci=off init=/init

  Notes:
    ro                 the image is shared; a writable mount would break sharing
    8250.nr_uarts=0    disables the serial device, which is tied to VMM stdout and can
                       otherwise consume unbounded host memory
    panic=1            a panicking guest exits rather than hanging and holding its cores
    init=/init         explicit, since there is no /sbin/init in this image

SUMMARY

ok "done"
