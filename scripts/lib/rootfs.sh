#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# Shared ext4 image creation for the rootfs builders.
# Source this file; do not execute it.
#
# Both build-rootfs.sh (minimal busybox image) and docker-to-rootfs.sh (converted container
# image) need the same four steps: create a sparse file, format it without a journal, mount it,
# and fsck it afterwards. The details are easy to get subtly wrong -- a journal wastes page
# cache on an image mounted read-only by every microVM on the host, and a non-sparse file
# allocates its full size on disk -- so they live in one place.

# Create and format a sparse ext4 image.
#
# Sparse because the image reports its full size but occupies only what is written, which matters
# when per-VM overlays are created from it at density. No journal because guests mount the image
# read-only, so a journal is dead weight in both image size and host page cache.
rootfs_create() {
  local out="$1" size_mib="$2" label="${3:-fcrootfs}"

  mkdir -p "$(dirname "$out")"
  rm -f "$out"

  log "creating ${size_mib} MiB sparse image at ${out}"
  dd if=/dev/zero of="$out" bs=1M count=0 seek="$size_mib" status=none

  log "formatting ext4 without a journal"
  mkfs.ext4 -q -O ^has_journal -L "$label" "$out"
  ok "image formatted"
}

# Mount an image on a fresh temporary directory and echo the mount point.
#
# The caller must arrange for rootfs_umount to run, normally via a trap: a leaked loop mount
# holds the image file and makes the next run fail with a confusing "device busy".
rootfs_mount() {
  local out="$1"
  local mnt
  mnt="$(mktemp -d)"
  mount -o loop "$out" "$mnt"
  printf '%s' "$mnt"
}

# Unmount and remove a mount point, tolerating either already being gone.
rootfs_umount() {
  local mnt="${1:-}"
  [[ -n "$mnt" ]] || return 0
  if mountpoint -q "$mnt" 2>/dev/null; then
    umount "$mnt" || true
  fi
  rmdir "$mnt" 2>/dev/null || true
}

# Write the identity files a booting guest expects.
#
# Some libc paths consult passwd and group even in a static binary. A converted container image
# usually has its own, so callers that already have them should skip this.
rootfs_write_identity() {
  local mnt="$1" hostname="${2:-fc-guest}"
  printf 'root:x:0:0:root:/:/bin/sh\n' > "$mnt/etc/passwd"
  printf 'root:x:0:\n' > "$mnt/etc/group"
  printf '%s\n' "$hostname" > "$mnt/etc/hostname"
  cat > "$mnt/etc/fstab" <<'FSTAB'
proc  /proc  proc  defaults  0 0
sysfs /sys   sysfs defaults  0 0
FSTAB
}

# Create the device nodes a guest needs.
#
# Neither a container image nor a from-scratch rootfs carries these: Docker populates /dev when it
# starts a container, and the guest agent does not mount devtmpfs. A surprising amount of ordinary
# software fails without /dev/null -- `git apply` inside a converted image died with "fatal: could
# not open '/dev/null' for reading and writing", and a test runner that redirects output would fail
# the same way, which under RUN_TASK would be recorded as a zero reward rather than a broken image.
#
# mknod needs the host's privileges, which every caller here already has.
rootfs_make_devices() {
  local mnt="$1"
  mkdir -p "$mnt/dev"
  # Mount point for devpts. Anything allocating a pseudo-terminal -- `expect`, `script`, an
  # interactive shell -- fails with "The system has no more ptys" without it. The agent does not
  # mount devpts today, so this only creates the directory, but a task that needs a pty fails
  # confusingly rather than obviously and the mount point costs nothing.
  mkdir -p "$mnt/dev/pts" "$mnt/dev/shm"
  mknod -m 666 "$mnt/dev/null" c 1 3 2>/dev/null || true
  mknod -m 666 "$mnt/dev/zero" c 1 5 2>/dev/null || true
  mknod -m 666 "$mnt/dev/random" c 1 8 2>/dev/null || true
  mknod -m 666 "$mnt/dev/urandom" c 1 9 2>/dev/null || true
  mknod -m 666 "$mnt/dev/tty" c 5 0 2>/dev/null || true
  # The console the kernel opens for PID 1's stdio. Absent, init starts with no standard streams.
  mknod -m 600 "$mnt/dev/console" c 5 1 2>/dev/null || true
  [[ -e "$mnt/dev/null" ]] || die "could not create /dev/null; the image would break most software"
  ok "device nodes created"
}

# Check the image is structurally sound.
#
# A corrupt image fails at guest boot with an opaque kernel panic rather than anything pointing
# back at the build, which is expensive to diagnose at density.
rootfs_fsck() {
  local out="$1"
  if command -v e2fsck >/dev/null 2>&1; then
    e2fsck -fp "$out" >/dev/null 2>&1 || warn "e2fsck reported issues; inspect ${out}"
    ok "filesystem check passed"
  fi
}

# Verify the image can actually boot: /init must exist and be executable.
#
# Without /init the kernel panics with "No init found", which is a confusing way to learn that a
# conversion dropped a file.
rootfs_assert_bootable() {
  local mnt="$1"
  [[ -e "$mnt/init" ]] || die "/init is missing; the kernel would panic with 'No init found'"
  [[ -x "$mnt/init" ]] || die "/init is not executable; the kernel would fail to exec it"
  ok "/init present and executable"
}
