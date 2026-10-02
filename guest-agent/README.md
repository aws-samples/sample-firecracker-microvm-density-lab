# Guest agent

The process that runs as PID 1 inside every microVM. It listens on an AF_VSOCK port, accepts
a workload request from the host over that channel, runs it, and reports timings back.

Rust, statically linked against musl, because it is resident in every microVM and its
resident set lands in every snapshot memory file. At 150 to 180 microVMs per host, a
megabyte here is multiplied by the fleet size, so the binary is size-optimised and carries a
single dependency (libc, a thin type-and-constant crate std already links).

## Protocol

One line of text in, one line out, over vsock port 5000. Text keeps the agent free of a
serialisation dependency and lets a failed run be reproduced by hand with `socat`.

| Request | Response |
|---------|----------|
| `PING` | `OK pong` |
| `RUN <iterations>` | `OK <iterations> <total_us> <mean_ns> <checksum>` |
| anything else | `ERR <message>` |

The checksum is returned so the host can confirm the work actually ran. Without it, an
optimiser that elided the compute loop would present as an implausibly fast host rather than
a bug.

The host reaches the agent through Firecracker's vsock bridge: connect to the Unix socket
Firecracker exposes, send `CONNECT 5000\n`, read `OK <hostport>\n`, then exchange the line
above.

## Build

```bash
./build.sh            # on a Linux host: produces a static musl binary
```

On Linux the default toolchain links a static binary directly. Cross-building from macOS
needs an LLVM linker (`brew install llvm`) wired up in `.cargo/config.toml`; the authoritative
build is on the Linux host, matching how the rest of the lab artifacts are built.

`cargo test` runs the logic tests (request parsing, the compute loop's resistance to being
optimised away, the sockaddr_vm layout) on any platform. The vsock and mount paths are
Linux-only and gated with `cfg`.

## Running as PID 1

As init the agent mounts /proc and /sys and reaps orphaned children. It never exits on a
recoverable error, because init exiting panics the kernel. Pair it with `panic=1` on the
guest kernel command line so a genuine panic exits the microVM promptly rather than leaving
it hung and holding its pinned cores.

## Status

**Booted and verified on real hardware (2026-08-01, m8i.metal-48xl).** A static build ran as
PID 1 in a Firecracker microVM (guest kernel 6.1.102, Firecracker v1.16.1), printed its
readiness line to the console, and answered `PING` and `RUN 5000000` over vsock. The RUN
reply carried a non-zero checksum, confirming the compute loop executed rather than being
optimised away.

> **Note on the validation kernel:** that run used guest kernel **6.1.102**, predating this
> repo's move to the pinned **6.18** (6.1's Firecracker support window ended 2026-09-02; see
> `scripts/build-guest-kernel.sh`). The agent uses no kernel-version-specific interfaces —
> vsock, `/proc`, `/sys`, and `execv` are stable across both lines — so the result is
> expected to carry over, but the 6.18 boot has not yet been re-run on hardware. Re-validate
> as part of the next lab session on metal.

Caveat on that build: it was static-linked against glibc (`+crt-static`,
`relocation-model=static`) because Amazon Linux 2023's packaged Rust does not ship the musl
std. musl remains the intended production target per `analysis/TECH-STACK.md`; the glibc
static build was sufficient to prove the boot path.

## Workloads

Two, selected by the request verb. They are not interchangeable and the reply tags differ so a
host cannot confuse them.

| Request | Reply | Working set | Measures |
|---|---|---|---|
| `RUN <iterations>` | `OK <iters> <total_us> <mean_ns> <checksum>` | ~16 bytes | Serial arithmetic throughput |
| `RUN_CACHE <ws_kib> <iterations>` | `OKC <ws_kib> <iters> <chase_us> <mean_ns> <setup_us> <checksum>` | `ws_kib` | Cache and memory latency |
| `RUN_SANDBOX <resident_kib> <steps> <write_kib>` | `OKS <resident_kib> <steps> <write_kib> <total_us> <spawn_us> <write_us> <spawns_ok> <checksum> <peak_rss_kib>` | `resident_kib` + writes | Fork/exec, resident memory, writeback |

`RUN` is the default for density sweeps. It is deliberately cache-insensitive, which makes it a
clean CPU-scheduling measurement and **useless for evaluating cache-affinity placement**: on a
host where all 96 cores share one L3, going from 10 to 90 concurrent microVMs moved its mean by
0.4%.

`RUN_CACHE` chases a permutation of `u32` indices where each step's address is the value just
loaded. Three properties are load-bearing:

- **Sattolo's algorithm, not Fisher-Yates.** The permutation must be a *single* cycle of full
  length. Fisher-Yates leaves several cycles, so a chase could land in a short one and stay
  resident in L1 regardless of buffer size, making the workload silently cache-insensitive.
  `build_cycle` is shared with its tests rather than duplicated, so a change to it cannot pass
  unnoticed.
- **Setup is outside the timed region**, and reported as `setup_us` so the exclusion is
  verifiable. Every page is touched before timing so first-touch faults are not counted as cache
  cost.
- **`black_box` after the loop, not inside it.** Inside would add a barrier per iteration and
  measure the barrier as much as the load.

The buffer is capped at 64 MiB. A larger request is refused with `ERR` rather than risking an OOM
kill, which would present to the host as a microVM that simply stopped answering.

### RUN_SANDBOX: the shape of an agentic episode

Neither `RUN` nor `RUN_CACHE` forks a process, holds memory, or writes a byte. A real RLVR rollout
does all three repeatedly: the model emits a tool call, a subprocess starts, it allocates and
writes, then the sandbox waits for the next token. Fork/exec, page-table setup, and writeback are
entirely different kernel paths from a register-resident loop, and they are the ones most likely to
contend at density because they take kernel locks and touch shared structures.

- `resident_kib` is held for the whole episode, faulted in at the start and re-touched every step
  so the kernel cannot reclaim it. **This is the parameter that decides whether memory or cores is
  the binding density constraint**: a SWE-bench-style sandbox needs 1-4 GiB, and at 2 GiB a 768 GiB
  host caps out near 380 microVMs regardless of core count.
- `steps` is tool calls. Each does a real `fork` + `execv` + `waitpid` on `/bin/busybox true`.
  Threads would be wrong here: they share the address space, so they skip the page-table copy and
  `mm` teardown that make fork expensive and that are the paths under test.
- `write_kib` is bytes written per step to `/tmp`, then removed. Removal is per step so a long
  episode cannot grow the tmpfs until the guest OOMs, which would look like a density limit while
  being a harness artifact.

Spawn and write times are reported separately from the total, so a density effect can be attributed
to process creation or to writeback rather than only observed in aggregate.

**Two properties worth understanding before reading the numbers.**

The agent mounts a **tmpfs on /tmp**, because the shared rootfs is read-only so one page-cache copy
can serve the fleet. **tmpfs pages are guest memory**, so writes consume the same budget as the
resident set. That is a real property of a read-only-rootfs sandbox design, not an artifact, and it
is why resident and write sizes are separate parameters rather than one number.

The resident set is **faulted in page by page**, not merely allocated. `vec![0u8; n]` can be served
by lazily-zeroed pages that never become resident, and pages that are not resident do not appear in
the host's `smaps_rollup` measurement -- so a 2 GiB guest would measure as much smaller and give a
confidently wrong answer to the memory question. `build_cycle`'s sibling `allocate_resident` is
extracted and tested by reading a sentinel back from every page. An earlier version of that test
inferred residency from *timing* and **passed with the fault-in loop deleted**, because `vec!`
already touches pages on some platforms. Verified by mutation.
