# Real tasks: SWE-bench and Terminal-Bench

The synthetic sandbox (`synthetic-sandbox.md`) is deterministic and dependency-free, which makes
it good for placement sweeps. But the point of the platform is to run *real* eval harnesses at
density. This note shows how to turn a real SWE-bench instance or Terminal-Bench task into a
bootable rootfs the guest agent can drive, using the scripts already in `../../scripts/`.

The result is a `rootfs.ext4` that answers the guest agent's `RUN_TASK` and `RUN_EPISODE` verbs
instead of (or in addition to) `RUN_SANDBOX`:

- `RUN_TASK` runs the baked verifier once and reports its exit code as the reward.
  Reply: `OKT <exit_code> <total_us> <setup_us> <peak_rss_kib>`.
- `RUN_EPISODE` replays a recorded agent trajectory as a full multi-turn episode, then runs the
  verifier. Reply: `OKE <turns> <turns_ok> <total_us> <turns_us> <verify_us> <exit_code> <peak_rss_kib>`.

The command and episode script live in `/etc/fc-task.env` inside the image, written at conversion
time, because the vsock protocol is whitespace-split and cannot carry a quoted argv.

## SWE-bench: a published per-instance image → rootfs

SWE-bench ships a container image per instance. `docker-to-rootfs.sh` flattens it, supplies the
guest agent as PID 1, and records the task command:

```sh
# From scripts/  (needs root and Docker; --dry-run needs neither)
./docker-to-rootfs.sh --image <swebench-instance-image-ref> --dry-run   # inspect first, writes nothing

sudo ./docker-to-rootfs.sh \
  --image <swebench-instance-image-ref> \
  --agent ../guest-agent/target/x86_64-unknown-linux-musl/release/guest-agent \
  --task-cmd 'python -m pytest -q' \
  --test-patch <instance-test-patch-file>
```

`docker export` (not `docker save`) is used because Firecracker boots a flat filesystem, not OCI
layers; the script reads back the image's `CMD`/`ENTRYPOINT`/`ENV`/`WORKDIR` with `docker inspect`
and writes them into `/etc/fc-task.env`. See the header of `../../scripts/docker-to-rootfs.sh`.

A helper to fetch a trajectory for the episode path:

```sh
./fetch-swe-agent-traj.sh ...      # see the script's header for arguments
```

## Terminal-Bench: a task directory → rootfs

Terminal-Bench does not publish per-task images; each task ships a `Dockerfile`, `tests/`, and a
`run-tests.sh` that the upstream harness builds and bind-mounts at run time. `tbench-to-rootfs.sh`
builds the image, stages what the harness would have mounted, rewrites `run-tests.sh` to run
offline (so density does not turn into one package-mirror egress per microVM), and hands off to
`docker-to-rootfs.sh`:

```sh
# From scripts/
./tbench-to-rootfs.sh --task <task-dir> --dry-run     # build and report, write no image, no root

sudo ./tbench-to-rootfs.sh \
  --task <terminal-bench-task-dir> \
  --agent ../guest-agent/target/x86_64-unknown-linux-musl/release/guest-agent \
  --out /opt/firecracker/rootfs-tbench.ext4
```

Fetch a task with `./fetch-tbench-task.sh` (see its header for arguments).

## Building the episode script for RUN_EPISODE

`RUN_EPISODE` replays a recorded trajectory rather than running the verifier once. Convert a
SWE-agent `.traj` file into a replayable episode script with `traj-to-episode.py`, then bake it
into the rootfs so `/etc/fc-task.env` points the guest agent at it:

```sh
# From scripts/
./traj-to-episode.py --traj <file.traj> --dry-run                        # print the plan, write nothing
./traj-to-episode.py --traj <file.traj> --workdir /testbed --out episode.sh
```

An episode is dominated by short process spawns and repeated reads over a warm page cache; a
verifier run is one long compute-bound process. They have different density behaviour, which is
why the guest agent exposes both verbs and the sweep can target either. See the header of
`../../scripts/traj-to-episode.py`.

## Dispatching to a converted image

Once the host agent is running with a converted rootfs
(`-rootfs=/opt/firecracker/rootfs-tbench.ext4`), dispatch the real workload the same way as the
synthetic one:

```sh
# From examples/
go run ./dispatch -vsock /srv/jailer/firecracker/vm0/root/v.sock -workload task
go run ./dispatch -vsock /srv/jailer/firecracker/vm0/root/v.sock -workload episode
```
