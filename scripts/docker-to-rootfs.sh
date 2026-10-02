#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
#
# Convert a container image into a bootable Firecracker rootfs.
#
# Written for SWE-bench's per-instance evaluation images, but nothing here is SWE-bench specific:
# any containerised eval harness can be moved onto microVMs this way, which is most of the value.
#
# # Why export rather than save
#
# `docker save` writes an OCI image: layers plus metadata, which Firecracker cannot boot.
# `docker export` flattens a container's filesystem to a tar, which is what a block device wants.
# The cost is that export drops the image's CMD, ENTRYPOINT, ENV and WORKDIR, so this script reads
# them back with `docker inspect` and writes them into /etc/fc-task.env for the guest agent.
#
# # What a container image lacks
#
#   no kernel   containers share the host's; supply one from build-guest-kernel.sh
#   no init     Docker runs CMD as PID 1; the guest agent takes that role instead
#   no size     an image has no fixed size, so the caller picks content x a headroom factor
#
# # Usage
#
#   sudo ./docker-to-rootfs.sh --image <ref> --agent <path> --task-cmd '<argv>' [--test-patch <f>]
#   ./docker-to-rootfs.sh --image <ref> --dry-run       # inspect only, writes nothing, no root
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"
# shellcheck source=lib/rootfs.sh
source "${SCRIPT_DIR}/lib/rootfs.sh"

IMAGE=""
AGENT_BIN=""
INSTANCE=""
OUT=""
MANIFEST=""
DRY_RUN=0
# The command that runs the instance's tests.
#
# This is required rather than derived, because a SWE-bench image's CMD is /bin/bash: upstream
# writes an eval script into the *running* container at evaluation time, so the test command is
# not in the image at all. Deriving it from CMD would bake `/bin/bash` in, and a shell that exits
# 0 immediately would report every task as a pass -- the exact inversion of the reward signal that
# RUN_TASK exists to measure.
TASK_CMD_OVERRIDE=""
# An episode script from traj-to-episode.py: the agent's recorded turns, then the verifier. Installed
# into the image so RUN_EPISODE can replay a full RL episode rather than only its last step.
EPISODE_SCRIPT=""
# Optional patch applied at conversion time.
#
# A SWE-bench instance's fail-to-pass test does not exist in the image: the repository is checked
# out at the base commit, and the test arrives with the instance's test_patch. Without it the
# runner passes trivially by running tests that already passed.
TEST_PATCH=""
# Headroom over measured content. A SWE-bench task applies a patch, may byte-compile, and writes
# test output, so the image needs room beyond what the container shipped with.
HEADROOM_PCT=30
# Platform is pinned because the published images are amd64 and Firecracker on EC2 metal is x86.
# On an arm64 workstation the pull and export still work (neither executes the image), so a
# conversion can be prepared anywhere.
PLATFORM="linux/amd64"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --image)      IMAGE="$2"; shift 2 ;;
    --agent)      AGENT_BIN="$2"; shift 2 ;;
    --instance)   INSTANCE="$2"; shift 2 ;;
    --out)        OUT="$2"; shift 2 ;;
    --manifest)   MANIFEST="$2"; shift 2 ;;
    --headroom)   HEADROOM_PCT="$2"; shift 2 ;;
    --platform)   PLATFORM="$2"; shift 2 ;;
    --task-cmd)   TASK_CMD_OVERRIDE="$2"; shift 2 ;;
    --test-patch) TEST_PATCH="$2"; shift 2 ;;
    --episode)    EPISODE_SCRIPT="$2"; shift 2 ;;
    --dry-run)    DRY_RUN=1; shift ;;
    -h|--help)   sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ -n "$IMAGE" ]] || die "--image is required, for example swebench/sweb.eval.x86_64.sympy_1776_sympy-20590:latest"
[[ "$HEADROOM_PCT" =~ ^[0-9]+$ ]] || die "--headroom must be a whole percentage, got $HEADROOM_PCT"
require_cmd docker

# Derive the instance ID from the image name when not given. Upstream encodes "__" as "_1776_" in
# image names, so django__django-11099 becomes ...django_1776_django-11099. Reverse that, since
# the instance ID is what appears in datasets and in the published leaderboard.
if [[ -z "$INSTANCE" ]]; then
  INSTANCE="$(basename "$IMAGE")"
  INSTANCE="${INSTANCE%%:*}"
  INSTANCE="${INSTANCE#sweb.eval.x86_64.}"
  INSTANCE="${INSTANCE//_1776_/__}"
  log "derived instance ID: ${INSTANCE}"
fi

section "1. Inspect the image"

# Pull only if absent, so a prepared workstation does not re-download gigabytes.
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  log "pulling ${IMAGE} (${PLATFORM})"
  docker pull --platform "$PLATFORM" "$IMAGE" \
    || die "pull failed. On arm64 the --platform flag is required, and the image must exist upstream."
fi

IMAGE_DIGEST="$(docker image inspect "$IMAGE" --format '{{index .RepoDigests 0}}' 2>/dev/null || echo "")"
[[ -n "$IMAGE_DIGEST" ]] || IMAGE_DIGEST="$(docker image inspect "$IMAGE" --format '{{.Id}}')"

# WORKDIR is part of the task definition, not a convenience: a test runner is generally only
# correct from its repository root. SWE-bench images set it to /testbed and their runners resolve
# test paths relative to it, so the guest agent chdirs there before exec.
WORKDIR="$(docker image inspect "$IMAGE" --format '{{.Config.WorkingDir}}')"
CMD_JSON="$(docker image inspect "$IMAGE" --format '{{json .Config.Cmd}}')"
ENTRYPOINT_JSON="$(docker image inspect "$IMAGE" --format '{{json .Config.Entrypoint}}')"
LAYER_SIZE_MIB=$(( $(docker image inspect "$IMAGE" --format '{{.Size}}') / 1024 / 1024 ))

log "entrypoint: ${ENTRYPOINT_JSON}"
log "cmd:        ${CMD_JSON}"
log "workdir:    ${WORKDIR:-/}"

# The image's CMD is deliberately not used as the task command. Upstream writes an eval script into
# the running container at evaluation time, so a SWE-bench image's CMD is just /bin/bash. Baking
# that in would give every task a shell that exits 0 and report a pass for work never done, which
# is the precise inversion of the reward the sweep exists to measure.
TASK_CMD="$TASK_CMD_OVERRIDE"
if [[ -z "$TASK_CMD" ]]; then
  warn "no --task-cmd given. The image's CMD ${CMD_JSON} is a shell, not a test command:"
  warn "upstream injects its eval script at run time, so it is not in the image at all."
  warn "Pass the runner explicitly, for example"
  warn "  --task-cmd '/opt/miniconda3/envs/testbed/bin/python bin/test -C --verbose <test file>'"
fi

section "2. Export the container filesystem"

# The export serves two purposes and so happens before the dry-run exit: it is the only accurate
# measure of how large the image needs to be, and it is the content itself.
#
# `docker image inspect .Size` cannot be used for sizing. It sums compressed layer sizes, and for
# the sympy instance reports 998 MiB against 2501 MiB of real content -- a 2.5x understatement that
# yields an image too small to extract into, failing partway through with a confusing ENOSPC.
TMPTAR=""
CONTAINER=""
MNT=""
cleanup() {
  rootfs_umount "${MNT:-}"
  [[ -n "$CONTAINER" ]] && docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  [[ -n "$TMPTAR" ]] && rm -f "$TMPTAR" || true
}
trap cleanup EXIT

# `docker create` without starting it: the filesystem is all that is wanted, and on an arm64
# workstation an amd64 image would not run anyway. Neither create nor export executes the image,
# which is why a conversion can be prepared on any architecture.
CONTAINER="$(docker create --platform "$PLATFORM" "$IMAGE" /bin/true)"

if (( DRY_RUN )); then
  # Piped to wc so a dry run measures the real content without writing gigabytes to disk.
  log "measuring flattened content (reads the whole image, writes nothing)"
  CONTENT_BYTES="$(docker export "$CONTAINER" | wc -c | tr -dc '0-9')"
else
  TMPTAR="$(mktemp "${TMPDIR:-/tmp}/docker-rootfs-XXXXXX.tar")"
  log "exporting to ${TMPTAR}"
  docker export "$CONTAINER" > "$TMPTAR"
  CONTENT_BYTES="$(wc -c < "$TMPTAR" | tr -dc '0-9')"
fi
[[ -n "$CONTENT_BYTES" && "$CONTENT_BYTES" -gt 0 ]] || die "docker export produced nothing"
CONTENT_MIB=$(( CONTENT_BYTES / 1024 / 1024 ))
ok "content ${CONTENT_MIB} MiB flattened (layer sizes report ${LAYER_SIZE_MIB} MiB)"

# Headroom over measured content, plus a fixed allowance. ext4 rounds every file up to a block, and
# an image with tens of thousands of small files loses real space to that, so the percentage alone
# is not enough on top of inode and metadata overhead.
SIZE_MIB=$(( CONTENT_MIB + CONTENT_MIB * HEADROOM_PCT / 100 + 256 ))
log "rootfs size: ${SIZE_MIB} MiB (content + ${HEADROOM_PCT}% + 256 MiB)"

# Guest memory is deliberately not derived from content size. The rootfs is attached read-only and
# demand-paged, so a 2.5 GiB image does not need 2.5 GiB of guest RAM -- only the pages the task
# touches. What the task actually needs is what RUN_TASK measures via VmHWM, so the honest advice is
# to start generously and read the measurement rather than to compute a floor from the image.
log "guest memory: start at 4096 MiB and size from the peak RSS that RUN_TASK reports"

if (( DRY_RUN )); then
  cat <<SUMMARY

  Dry run, nothing written.

  Instance:   ${INSTANCE}
  Digest:     ${IMAGE_DIGEST}
  Content:    ${CONTENT_MIB} MiB flattened
  Rootfs:     ${SIZE_MIB} MiB
  Workdir:    ${WORKDIR:-/}
  Task:       ${TASK_CMD:-<none: pass --task-cmd>}

  Re-run with --agent <path> --task-cmd '<argv>' and without --dry-run to build.

SUMMARY
  exit 0
fi

# One of the two is required. An episode script carries its own verifier as the last step, so an
# image built for RUN_EPISODE needs no separate task command; an image built for RUN_TASK does.
if [[ -z "$TASK_CMD" && -z "$EPISODE_SCRIPT" ]]; then
  die "--task-cmd or --episode is required: the image's CMD is a shell, not a test command"
fi
# The command's first word must be an absolute path. The guest agent execs the task with execv,
# which takes a path and does not search PATH, so a bare name such as "bash" or "python" exits 127
# however complete the image is. That failure only shows up on hardware, as an exit code with no
# message, and the resulting millisecond "task" reads as a fast success rather than an error.
if [[ -n "$TASK_CMD" ]]; then
  case "${TASK_CMD%% *}" in
    /*) ;;
    *) die "task command '${TASK_CMD}' does not start with an absolute path. The guest runs it with
execv, which does not search PATH, so it would exit 127. Use the full path, for example
/usr/bin/bash rather than bash." ;;
  esac
fi
[[ -n "$AGENT_BIN" ]] || die "--agent is required: the guest agent runs as PID 1"
[[ -f "$AGENT_BIN" ]] || die "agent binary not found: $AGENT_BIN"
# A dynamically linked agent needs its interpreter and libraries at the exact paths it was built
# against. A converted image may carry a different libc, so static linking is required.
if command -v file >/dev/null 2>&1 && file "$AGENT_BIN" | grep -q "dynamically linked"; then
  die "agent must be statically linked (build with +crt-static)"
fi
[[ -z "$TEST_PATCH" || -f "$TEST_PATCH" ]] || die "test patch not found: $TEST_PATCH"

OUT="${OUT:-${ARTIFACT_DIR}/rootfs-${INSTANCE}.ext4}"
MANIFEST="${MANIFEST:-${OUT%.ext4}.manifest.json}"

require_root
require_cmd mkfs.ext4
require_cmd tar

section "3. Build the image"
rootfs_create "$OUT" "$SIZE_MIB" "fc-${INSTANCE:0:11}"
MNT="$(rootfs_mount "$OUT")"

log "extracting into the image"
# --numeric-owner keeps uids as they were rather than remapping through the host's passwd, which
# matters because the task may expect to run as a specific non-root user.
tar -xf "$TMPTAR" -C "$MNT" --numeric-owner \
  || die "extraction failed; is ${SIZE_MIB} MiB enough for ${CONTENT_MIB} MiB of content?"
ok "extracted"

# Directories a booting guest needs that a container image may not carry, since Docker provides
# them at runtime rather than in the image.
mkdir -p "$MNT"/{proc,sys,dev,run,tmp}
chmod 1777 "$MNT/tmp"

# Docker populates /dev when it starts a container, so an exported filesystem has an empty one.
rootfs_make_devices "$MNT"

# The instance's fail-to-pass test is not in the image.
#
# A SWE-bench image holds the repository at the base commit, and the test that distinguishes a
# solved instance from an unsolved one arrives separately in the instance's test_patch. Without it
# the runner executes only tests that already passed, so every task reports exit 0 and the reward
# signal is a constant. Applying it here rather than in the guest keeps the microVM's measured work
# to the test run itself.
if [[ -n "$TEST_PATCH" ]]; then
  section "3b. Apply the instance test patch"
  require_cmd git
  # Absolute path, because git runs with -C inside the mounted image and a relative path would
  # resolve against that rather than the caller's directory.
  PATCH_ABS="$(cd "$(dirname "$TEST_PATCH")" && pwd)/$(basename "$TEST_PATCH")"
  # git apply rather than patch: the dataset ships git-produced unified diffs, and a
  # whitespace-sensitive failure is better reported than silently fuzzed into the wrong place.
  # Status captured directly rather than through a pipe, so the check cannot read tail's exit code.
  if PATCH_OUT="$(git -C "${MNT}${WORKDIR:-/}" apply --verbose "$PATCH_ABS" 2>&1)"; then
    ok "test patch applied"
  else
    printf '%s\n' "$PATCH_OUT" | tail -3 >&2
    die "test patch did not apply. Is the image's checkout at the instance's base_commit?"
  fi
fi

section "4. Install the agent and task metadata"
# The agent replaces Docker's CMD as PID 1. Any existing /init is preserved under a different
# name rather than clobbered, so a converted image remains inspectable.
if [[ -e "$MNT/init" && ! -L "$MNT/init" ]]; then
  mv "$MNT/init" "$MNT/init.original"
  warn "image had its own /init; kept as /init.original"
fi
install -m 0755 "$AGENT_BIN" "$MNT/init"
rootfs_assert_bootable "$MNT"

# The episode script, if one was given. Installed at a fixed path so the env file can name it
# without the caller having to know the image's layout.
EPISODE_PATH_IN_IMAGE="/episode.sh"
BASH_IN_IMAGE=""
if [[ -n "$EPISODE_SCRIPT" ]]; then
  [[ -f "$EPISODE_SCRIPT" ]] || die "episode script not found: $EPISODE_SCRIPT"
  install -m 0755 "$EPISODE_SCRIPT" "${MNT}${EPISODE_PATH_IN_IMAGE}"
  # Resolve the interpreter inside the image, absolute. The guest execs it with execv, which does
  # not search PATH, so a bare "bash" exits 127 with bash sitting in the image -- a failure that
  # only shows up on hardware, as an exit code with no message.
  for candidate in /usr/bin/bash /bin/bash /usr/bin/sh /bin/sh; do
    if [[ -x "${MNT}${candidate}" ]]; then
      BASH_IN_IMAGE="$candidate"
      break
    fi
  done
  [[ -n "$BASH_IN_IMAGE" ]] || die "no shell found in the image; the episode script needs one"
  ok "installed episode script, interpreter ${BASH_IN_IMAGE}"
fi

# This file is the whole reason the conversion is not just a tar extract: docker export discards
# CMD, ENTRYPOINT, ENV and WORKDIR, and the guest agent needs both the command and the directory to
# run it from.
{
  printf '# Written by docker-to-rootfs.sh. Consumed by the guest agent.\n'
  printf '#   FC_TASK_*    -> RUN_TASK, one verifier run\n'
  printf '#   FC_EPISODE_* -> RUN_EPISODE, a replayed trajectory: many turns, then the verifier\n'
  printf 'FC_TASK_INSTANCE=%s\n' "$INSTANCE"
  # Only emitted when set. An empty FC_TASK_CMD would make an episode-only image look like a task
  # image whose command went missing. Written as if-blocks rather than `[[ ]] &&` because a false
  # test returns non-zero, which under `set -e` would abort the whole group.
  if [[ -n "$TASK_CMD" ]]; then
    printf 'FC_TASK_CMD=%s\n' "$TASK_CMD"
  fi
  printf 'FC_TASK_WORKDIR=%s\n' "${WORKDIR:-/}"
  printf 'FC_TASK_IMAGE=%s\n' "$IMAGE"
  if [[ -n "$TEST_PATCH" ]]; then
    printf 'FC_TASK_TEST_PATCH=applied\n'
  fi
  # Episode keys, read by the RUN_EPISODE verb. The interpreter is stored with the script because
  # the guest execs it with execv, which does not search PATH.
  if [[ -n "$EPISODE_SCRIPT" ]]; then
    printf 'FC_EPISODE_SCRIPT=%s\n' "$EPISODE_PATH_IN_IMAGE"
    printf 'FC_EPISODE_SHELL=%s\n' "$BASH_IN_IMAGE"
  fi
} > "$MNT/etc/fc-task.env"
ok "wrote /etc/fc-task.env"

# Measured after extraction, so it reflects what ext4 actually charges rather than the tar's size.
DISK_MIB="$(du -sm "$MNT" | cut -f1)"
rootfs_umount "$MNT"
MNT=""
rootfs_fsck "$OUT"

section "5. Manifest"
# Provenance travels with the artifact. A converted image is one more thing that can drift, and a
# result quoted from an image nobody can identify is not defensible in review.
cat > "$MANIFEST" <<JSON
{
  "instance_id": "${INSTANCE}",
  "image": "${IMAGE}",
  "image_digest": "${IMAGE_DIGEST}",
  "platform": "${PLATFORM}",
  "layer_size_mib": ${LAYER_SIZE_MIB},
  "content_size_mib": ${CONTENT_MIB},
  "on_disk_size_mib": ${DISK_MIB},
  "rootfs_size_mib": ${SIZE_MIB},
  "task_command": "${TASK_CMD}",
  "task_workdir": "${WORKDIR:-/}",
  "test_patch_applied": $( [[ -n "$TEST_PATCH" ]] && echo true || echo false ),
  "converted_by": "docker-to-rootfs.sh"
}
JSON
ok "wrote ${MANIFEST}"

cat <<SUMMARY

  Rootfs:     ${OUT}
  Manifest:   ${MANIFEST}
  Instance:   ${INSTANCE}
  Content:    ${DISK_MIB} MiB on disk in a ${SIZE_MIB} MiB image
  Workdir:    ${WORKDIR:-/}
  Task:       ${TASK_CMD}

  Boot one microVM and run the task. Start generously and measure: RUN_TASK reports peak RSS, and
  that measurement is what should size the fleet, not this guess.

    labsweep -task-instance ${INSTANCE} -configs B -densities 1 -repeats 1 \\
      -rootfs ${OUT} -mem 4096

  The image is attached read-only so one host page-cache copy serves the whole fleet. A task that
  writes needs a per-VM overlay; the guest agent mounts a tmpfs on /tmp for the rest.

    ro console= 8250.nr_uarts=0 reboot=k panic=1 pci=off init=/init

SUMMARY

ok "done"
