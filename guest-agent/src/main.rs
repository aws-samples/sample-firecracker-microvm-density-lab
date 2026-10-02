// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
//! Minimal in-microVM agent for density measurement.
//!
//! Runs as PID 1. Listens on an AF_VSOCK port, accepts a workload request from the host,
//! runs it, and reports timings back over the same connection.
//!
//! # Why this is deliberately tiny
//!
//! The agent is resident in every microVM and its resident set lands in every snapshot
//! memory file. At 150 to 180 microVMs per host, a megabyte here is 150 to 180 megabytes of
//! host memory that could otherwise hold more microVMs. So: one dependency (libc, a thin
//! type-and-constant crate that std already links), a size-optimised release profile, and no
//! functionality beyond what the measurement needs.
//!
//! # Platform
//!
//! Linux only, and built for a musl target so the result is a static binary the rootfs
//! builder can drop in as /init with no interpreter or shared libraries. It will not compile
//! on macOS: some libc signatures differ, `mount` among them.
//!
//! # Running as PID 1
//!
//! As init, this process is responsible for mounting /proc and /sys and for reaping
//! children. If it exits, the kernel panics, so the main loop never returns on a recoverable
//! error. With `panic=1` on the kernel command line, a panic exits the guest promptly rather
//! than leaving it hung and holding its pinned cores.

use std::io::{BufRead, BufReader, Write};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::time::Instant;

/// vsock port the agent listens on. The host sends "CONNECT <port>\n" to the Unix socket
/// Firecracker exposes, and Firecracker forwards to whatever is listening here.
const AGENT_PORT: u32 = 5000;

/// AF_VSOCK, from the kernel's socket address families.
const AF_VSOCK: libc::c_int = 40;

/// Accept from any CID. The guest does not know or care which CID the host uses.
const VMADDR_CID_ANY: u32 = 0xffff_ffff;

fn main() {
    let is_init = std::process::id() == 1;

    if is_init {
        // As init, nothing else has mounted these, and /proc in particular is needed for
        // any self-inspection.
        mount_pseudo_filesystems();
    }

    let listener = match VsockListener::bind(AGENT_PORT) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("fc-guest-agent: cannot bind vsock port {AGENT_PORT}: {e}");
            // As init, exiting panics the kernel. Sleep instead so the host sees a guest
            // that booted but never became ready, which is a clearer signal than a panic.
            if is_init {
                loop {
                    std::thread::sleep(std::time::Duration::from_secs(3600));
                }
            }
            std::process::exit(1);
        }
    };

    // A single line on stdout is the readiness signal. The host sees it in the VMM log when
    // logging is enabled. Keep it to one line: guest stdout is tied to the serial device,
    // and unbounded output there can consume host memory.
    println!("fc-guest-agent ready on vsock port {AGENT_PORT}");
    let _ = std::io::stdout().flush();

    loop {
        match listener.accept() {
            Ok(stream) => {
                if let Err(e) = handle(stream) {
                    eprintln!("fc-guest-agent: request failed: {e}");
                }
            }
            Err(e) => {
                eprintln!("fc-guest-agent: accept failed: {e}");
                // Do not exit: as init that would panic the kernel, and a transient accept
                // failure should not end the microVM's life.
                std::thread::sleep(std::time::Duration::from_millis(50));
            }
        }
        if is_init {
            reap_children();
        }
    }
}

/// Handle one request.
///
/// The wire format is one line of text in, one line of text out. A text protocol keeps the
/// agent free of a serialisation dependency, and makes a failed run diagnosable by hand with
/// socat rather than requiring a decoder.
///
/// Requests:  `PING`, `RUN <iterations>`, `RUN_CACHE <ws_kib> <iterations>`,
///            `RUN_SANDBOX <resident_kib> <steps> <write_kib>`, `RUN_TASK`, `RUN_EPISODE`
/// Responses: `OK  <iterations> <total_us> <mean_ns> <checksum>`
///            `OKC <ws_kib> <iterations> <chase_us> <mean_ns> <setup_us> <checksum>`
///            `OKS <resident_kib> <steps> <write_kib> <total_us> <spawn_us> <write_us> <spawns_ok> <checksum> <peak_rss_kib>`
///            `OKT <exit_code> <total_us> <setup_us> <peak_rss_kib>`
///            `OKE <turns> <turns_ok> <total_us> <turns_us> <verify_us> <exit_code> <peak_rss_kib>`
///            `ERR <message>`
///
/// Every workload has its own reply tag rather than extra fields on a shared one. Field
/// positions differ between them, so a shared tag would let a host record one workload's
/// numbers under another's label and compare measurements that are not comparable. The host
/// checks the tag against what it asked for and refuses the cell on a mismatch.
fn handle(stream: VsockStream) -> std::io::Result<()> {
    let mut reader = BufReader::new(stream.try_clone()?);
    let mut line = String::new();
    reader.read_line(&mut line)?;

    let mut out = stream;
    let response = match parse_request(line.trim()) {
        Some(Request::Ping) => "OK pong\n".to_string(),
        Some(Request::Run { iterations }) => {
            let started = Instant::now();
            let checksum = compute_loop(iterations);
            let elapsed = started.elapsed();
            let mean_ns = if iterations > 0 {
                elapsed.as_nanos() / iterations as u128
            } else {
                0
            };
            // The checksum is returned so the host can confirm the work actually ran. Without
            // it, an optimiser that elided the loop would look like an implausibly fast host.
            format!(
                "OK {} {} {} {}\n",
                iterations,
                elapsed.as_micros(),
                mean_ns,
                checksum
            )
        }
        Some(Request::RunCache {
            working_set_kib,
            iterations,
        }) => match run_cache(working_set_kib, iterations) {
            Ok(r) => format!(
                "OKC {} {} {} {} {} {}\n",
                working_set_kib,
                iterations,
                r.chase.as_micros(),
                r.mean_ns,
                r.setup.as_micros(),
                r.checksum
            ),
            Err(e) => format!("ERR {e}\n"),
        },
        Some(Request::RunSandbox {
            resident_kib,
            steps,
            write_kib,
        }) => match run_sandbox(resident_kib, steps, write_kib) {
            Ok(r) => format!(
                "OKS {} {} {} {} {} {} {} {} {}\n",
                resident_kib,
                steps,
                write_kib,
                r.total.as_micros(),
                r.spawn.as_micros(),
                r.write.as_micros(),
                r.spawns_ok,
                r.checksum,
                r.peak_rss_kib
            ),
            Err(e) => format!("ERR {e}\n"),
        },
        Some(Request::RunTask) => match run_task() {
            Ok(r) => format!(
                "OKT {} {} {} {}\n",
                r.exit_code,
                r.total.as_micros(),
                r.setup.as_micros(),
                r.peak_rss_kib
            ),
            Err(e) => format!("ERR {e}\n"),
        },
        Some(Request::RunEpisode) => match run_episode() {
            Ok(r) => format!(
                "OKE {} {} {} {} {} {} {}\n",
                r.turns,
                r.turns_ok,
                r.total.as_micros(),
                r.turns_us,
                r.verify_us,
                r.exit_code,
                r.peak_rss_kib
            ),
            Err(e) => format!("ERR {e}\n"),
        },
        None => format!("ERR unrecognised request: {}\n", line.trim()),
    };

    out.write_all(response.as_bytes())?;
    out.flush()
}

enum Request {
    Ping,
    Run {
        iterations: u64,
    },
    /// Pointer-chase over a buffer of the given size, to make the workload cache-sensitive.
    RunCache {
        working_set_kib: u64,
        iterations: u64,
    },
    /// Agentic-sandbox shaped: fork/exec churn, resident memory, and file writes.
    RunSandbox {
        resident_kib: u64,
        steps: u64,
        write_kib: u64,
    },
    /// Run the real task baked into this image and report its exit status as the reward.
    RunTask,
    /// Replay a recorded agent trajectory as a full RL episode: many turns, then one verifier run.
    RunEpisode,
}

fn parse_request(line: &str) -> Option<Request> {
    let mut parts = line.split_whitespace();
    match parts.next()? {
        "PING" => Some(Request::Ping),
        "RUN" => {
            let n: u64 = parts.next()?.parse().ok()?;
            Some(Request::Run { iterations: n })
        }
        "RUN_CACHE" => {
            let kib: u64 = parts.next()?.parse().ok()?;
            let n: u64 = parts.next()?.parse().ok()?;
            Some(Request::RunCache {
                working_set_kib: kib,
                iterations: n,
            })
        }
        // No arguments: the command lives in /etc/fc-task.env, written at image conversion.
        // Passing it over the wire would need shell quoting in a whitespace-split protocol.
        "RUN_TASK" => Some(Request::RunTask),
        // Also argument-free: the episode script is baked into the image at conversion, and its
        // path comes from /etc/fc-task.env.
        "RUN_EPISODE" => Some(Request::RunEpisode),
        "RUN_SANDBOX" => {
            let resident_kib: u64 = parts.next()?.parse().ok()?;
            let steps: u64 = parts.next()?.parse().ok()?;
            let write_kib: u64 = parts.next()?.parse().ok()?;
            Some(Request::RunSandbox {
                resident_kib,
                steps,
                write_kib,
            })
        }
        _ => None,
    }
}

/// Largest working set the agent will allocate, in KiB.
///
/// The guest is configured with 128 MiB by default, and the buffer must coexist with the
/// kernel, page tables, and the agent itself. 64 MiB leaves headroom; asking for more is
/// refused with an error rather than risking an OOM kill that would present to the host as a
/// microVM that stopped answering, which is a far harder failure to diagnose.
const MAX_WORKING_SET_KIB: u64 = 64 * 1024;

struct CacheResult {
    /// Time spent chasing pointers. This is the measurement.
    chase: std::time::Duration,
    /// Time spent allocating and shuffling. Reported separately so it can be confirmed to be
    /// outside the measurement rather than assumed to be.
    setup: std::time::Duration,
    mean_ns: u128,
    checksum: u64,
}

/// A cache-sensitive workload: dependent-load pointer chase over a buffer.
///
/// # Why a pointer chase
///
/// `compute_loop` has a working set of about sixteen bytes, so it lives entirely in registers
/// and never touches last-level cache. Measured on real hardware, taking an m8i host from 10 to
/// 90 concurrent microVMs moved its mean by 0.4% even though all 90 shared a single L3. A
/// workload that cannot see cache pressure cannot be used to evaluate cache-affinity placement:
/// it would report "no difference" regardless of whether the placement worked.
///
/// This traverses a permutation of indices where each step's address depends on the previous
/// load's value. That serialises the loads, so the loop runs at the latency of whichever cache
/// level holds the buffer rather than at memory bandwidth, and hardware prefetchers cannot
/// predict the next address. Time per iteration therefore steps up as the working set outgrows
/// L2 and then the L3 share available to the core.
///
/// # Interpreting the working set
///
/// On a host with N cores sharing an L3 of size S, a core's fair share is roughly S/N. Sweeping
/// the working set across that boundary is what distinguishes a real cache-affinity effect from
/// noise: the effect should appear only when the working set is near the share, and vanish both
/// below it (fits L2 regardless of placement) and far above it (misses regardless).
fn run_cache(working_set_kib: u64, iterations: u64) -> Result<CacheResult, String> {
    if working_set_kib == 0 {
        return Err("working set must be greater than zero".to_string());
    }
    if working_set_kib > MAX_WORKING_SET_KIB {
        return Err(format!(
            "working set {working_set_kib} KiB exceeds the {MAX_WORKING_SET_KIB} KiB limit"
        ));
    }

    let setup_start = Instant::now();

    // u32 indices rather than pointers or u64: half the memory per entry for the same number of
    // distinct cache lines touched, so a given working set covers twice the footprint.
    let entries = (working_set_kib as usize * 1024) / std::mem::size_of::<u32>();
    if entries < 2 {
        return Err("working set too small to form a cycle".to_string());
    }
    let ring = build_cycle(entries);

    // Touch every page before timing. Without this the first traversal would also be paying
    // for demand-zero page faults, which is a first-touch cost rather than a cache cost. The
    // m8a warmup artifact found on 2026-08-01 is exactly this effect leaking into a
    // measurement.
    for v in ring.iter() {
        std::hint::black_box(*v);
    }

    let setup = setup_start.elapsed();

    let chase_start = Instant::now();
    let mut idx: u32 = 0;
    let mut checksum: u64 = 0;
    for _ in 0..iterations {
        // The load address depends on the value just loaded, so the CPU cannot run ahead.
        idx = ring[idx as usize];
        checksum = checksum.wrapping_add(idx as u64);
    }
    // black_box after the loop, not inside: inside would add a barrier per iteration and
    // measure the barrier as much as the load. The checksum plus this call is enough to keep
    // the optimiser from removing the traversal.
    std::hint::black_box(idx);
    let chase = chase_start.elapsed();

    let mean_ns = if iterations > 0 {
        chase.as_nanos() / iterations as u128
    } else {
        0
    };

    Ok(CacheResult {
        chase,
        setup,
        mean_ns,
        checksum,
    })
}

/// Largest resident buffer the sandbox workload will allocate, in KiB.
///
/// Higher than MAX_WORKING_SET_KIB because the whole point of this workload is to test guest
/// sizes a real sandbox needs (1-4 GiB), where memory rather than cores may be the binding
/// density constraint. Still bounded: a request beyond this is refused with an error rather
/// than risking an OOM kill, which would present to the host as a microVM that silently
/// stopped answering.
const MAX_RESIDENT_KIB: u64 = 6 * 1024 * 1024; // 6 GiB

/// Directory the workload writes into.
///
/// The shared rootfs is mounted read-only so that one page-cache copy serves the whole fleet,
/// which means a sandbox that writes anything needs a writable mount. /tmp is a tmpfs the agent
/// mounts at boot. **tmpfs pages are guest memory**, so writes here count against the same
/// budget as the resident buffer. That is not an artifact of this harness: it is the real
/// constraint any read-only-rootfs sandbox design faces, and it is why write_kib is a separate
/// parameter rather than folded into resident_kib.
const WORK_DIR: &str = "/tmp";

/// Debug is derived so a test asserting a refusal can print what it got instead.
#[derive(Debug)]
struct SandboxResult {
    /// Wall clock for the whole episode. This is the headline measurement.
    total: std::time::Duration,
    /// Time inside fork/exec, aggregated. Reported separately because process creation is the
    /// path an arithmetic loop never exercises and the one most likely to behave differently
    /// at density.
    spawn: std::time::Duration,
    /// Time inside file writes, aggregated.
    write: std::time::Duration,
    /// Child processes that were successfully reaped. A shortfall means fork failed, which at
    /// density usually means a PID or memory limit was hit, and is a result rather than an
    /// error to hide.
    spawns_ok: u64,
    checksum: u64,
    /// Peak resident set of *this process*, from `/proc/self/status` VmHWM.
    ///
    /// The agent's own footprint, which is the opposite of the right source for `RUN_TASK` and
    /// `RUN_EPISODE` -- see the note on `spawn_and_wait_full`, where the memory belongs to a child
    /// and `wait4`'s rusage is what moves. Here the relationship is inverted: `run_sandbox`
    /// allocates the resident buffer in the agent itself and spawns only `busybox true`, so a
    /// child's rusage would report a few hundred KiB no matter how large the buffer is.
    ///
    /// Without this the sandbox workload was the one place a declared resident set had no
    /// corresponding measurement anywhere: host-side memory was recorded before the workload ran,
    /// and the reply carried no peak, so a guest told to hold 5 GiB was recorded at 237 MiB.
    peak_rss_kib: u64,
}

/// Peak resident set of this process in KiB, from `/proc/self/status` VmHWM.
///
/// VmHWM rather than `getrusage(RUSAGE_SELF)`: both report the high-water mark, but VmHWM is
/// readable without a syscall wrapper and is the field `/proc` documents for this purpose. Returns
/// 0 if the field cannot be read, which reads as "not measured" rather than as "no memory used" --
/// a wrong small number here would look like a real result.
fn self_peak_rss_kib() -> u64 {
    let status = match std::fs::read_to_string("/proc/self/status") {
        Ok(s) => s,
        Err(_) => return 0,
    };
    for line in status.lines() {
        if let Some(rest) = line.strip_prefix("VmHWM:") {
            for tok in rest.split_whitespace() {
                if let Ok(v) = tok.parse::<u64>() {
                    return v;
                }
            }
        }
    }
    0
}

/// A workload shaped like one agentic RL episode.
///
/// # Why the compute loop is not enough
///
/// `compute_loop` is arithmetic in registers and `run_cache` is memory reads. Neither forks a
/// process, allocates lasting memory, or writes a byte. A real RLVR rollout does all three
/// repeatedly: the model emits a tool call, the harness spawns a subprocess (interpreter,
/// compiler, test runner), that process allocates and writes, and then the sandbox waits for
/// the next token. Fork/exec, page-table setup, and writeback are entirely different kernel
/// paths from a register-resident loop, and they are the paths most likely to contend at
/// density because they take kernel locks and touch shared structures.
///
/// # What each parameter models
///
/// - `resident_kib`: memory held for the whole episode, faulted in and re-touched each step.
///   Stands in for an interpreter's heap plus a loaded repo. **This is the parameter that
///   answers whether memory binds before cores**: a SWE-bench-style sandbox needs 1-4 GiB, and
///   at 2 GiB a 768 GiB host caps out near 380 microVMs regardless of core count.
/// - `steps`: tool calls in the episode. Each spawns a child, so this drives fork/exec churn.
/// - `write_kib`: bytes written per step, to a tmpfs. Models file edits and test output.
///
/// # Deliberate simplifications
///
/// There is no Python interpreter, no compiler, and no repository. Those need a full distro
/// image (the `docker-to-rootfs` deliverable) and would make this a much larger task. What is
/// modelled is the *kernel-visible shape* of an episode: process creation, resident memory, and
/// writeback. Absolute times will not match a real episode. The point is to find out whether
/// those paths change the density picture that the arithmetic loop produced.
fn run_sandbox(resident_kib: u64, steps: u64, write_kib: u64) -> Result<SandboxResult, String> {
    if resident_kib > MAX_RESIDENT_KIB {
        return Err(format!(
            "resident {resident_kib} KiB exceeds the {MAX_RESIDENT_KIB} KiB limit"
        ));
    }

    let started = Instant::now();
    let mut spawn_total = std::time::Duration::ZERO;
    let mut write_total = std::time::Duration::ZERO;
    let mut spawns_ok: u64 = 0;
    let mut checksum: u64 = 0;

    // Allocate and fault in the resident set once, before the loop. A real sandbox pays this
    // at startup, not per tool call.
    let resident_bytes = (resident_kib as usize).saturating_mul(1024);
    let mut resident = allocate_resident(resident_bytes);
    std::hint::black_box(&resident);

    for step in 0..steps {
        // --- Tool call: spawn a child process ---
        //
        // busybox true is the cheapest real fork+exec available in this image. Using a
        // thread instead would miss the point entirely: threads share the address space, so
        // they skip the page-table copy and mm teardown that make fork expensive and that are
        // the paths under test.
        let spawn_start = Instant::now();
        match spawn_child() {
            Ok(()) => spawns_ok += 1,
            Err(e) => {
                // A fork failure at density is a finding, not a crash. Report what was
                // achieved so the host can see the shortfall.
                return Err(format!(
                    "fork/exec failed at step {step} after {spawns_ok} successful spawns: {e}"
                ));
            }
        }
        spawn_total += spawn_start.elapsed();

        // --- Re-touch the resident set ---
        //
        // A real episode keeps its heap warm across tool calls. Without this the kernel could
        // reclaim the pages and the "resident" figure would be fiction. Strided so the cost
        // stays modest while every page is visited.
        if resident_bytes > 0 {
            let stride = 4096;
            let mut i = (step as usize * stride) % resident_bytes;
            let mut visited = 0usize;
            while visited < resident_bytes / stride {
                resident[i] = resident[i].wrapping_add(1);
                checksum = checksum.wrapping_add(resident[i] as u64);
                i = (i + stride) % resident_bytes;
                visited += 1;
            }
        }

        // --- File write ---
        if write_kib > 0 {
            let write_start = Instant::now();
            write_step(step, write_kib)?;
            write_total += write_start.elapsed();
        }
    }

    // Keep the resident set alive to here, or the optimiser could free it early and the memory
    // measurement the host takes would miss it.
    std::hint::black_box(&resident);

    // Read the peak before `resident` is dropped at the end of this function. VmHWM is a
    // high-water mark so it would survive the drop, but reading it here keeps the measurement
    // adjacent to the allocation it describes.
    let peak_rss_kib = self_peak_rss_kib();

    Ok(SandboxResult {
        total: started.elapsed(),
        spawn: spawn_total,
        write: write_total,
        spawns_ok,
        checksum,
        peak_rss_kib,
    })
}

/// Page size assumed for the fault-in stride. 4 KiB on x86-64, which is the only target.
const PAGE_SIZE: usize = 4096;

/// Sentinel written into the first byte of every page during fault-in.
///
/// A distinctive non-zero value rather than 1, so a test can distinguish "this page was
/// deliberately touched" from "this page happens to be zero because it was never touched".
const RESIDENT_MARK: u8 = 0x5a;

/// Allocate a buffer and fault in every page.
///
/// `vec![0u8; n]` does not guarantee residency: the allocator may hand back lazily-zeroed pages
/// that only materialise on first touch. If that happened, the host's `smaps_rollup` measurement
/// would report a fraction of the configured size, and the memory-ceiling question this workload
/// exists to answer would get a wrong answer that looked plausible.
///
/// Extracted from `run_sandbox` so a test can assert residency **directly**, by reading the
/// sentinel back, rather than inferring it from how long allocation took. Timing is not a valid
/// proxy here: on some platforms `vec!` already touches the pages, so a timing-based test passes
/// even when the fault-in loop is deleted. That was verified by mutation.
fn allocate_resident(bytes: usize) -> Vec<u8> {
    let mut buf: Vec<u8> = vec![0u8; bytes];
    for i in (0..bytes).step_by(PAGE_SIZE) {
        buf[i] = RESIDENT_MARK;
    }
    buf
}

/// fork + exec + wait, using raw syscalls.
///
/// `std::process::Command` would work, but it allocates, builds environment arrays, and sets up
/// pipes, so the measurement would include std's bookkeeping alongside the kernel cost. Raw
/// fork/execve/waitpid keeps the timed region close to the syscalls under test.
/// A reaped child: its shell-convention exit code and its own peak resident set in KiB.
#[derive(Debug, Clone, Copy)]
struct ChildOutcome {
    exit_code: i32,
    peak_rss_kib: u64,
}

#[cfg(target_os = "linux")]
fn spawn_child() -> Result<(), String> {
    // busybox true is the cheapest real fork+exec in this image, which is what the sandbox
    // workload wants: it measures process-creation cost, not the child's work.
    spawn_and_wait("/bin/busybox", &["busybox", "true"], None).map(|_| ())
}

/// fork + execv + wait4, returning the child's exit code and peak resident set.
///
/// Shared by the sandbox workload (which discards the outcome) and RUN_TASK (for which the exit
/// code *is* the measurement, since a SWE-bench instance's fail-to-pass status is the RLVR reward,
/// and the peak RSS is what warm-pool sizing needs).
///
/// `wait4` rather than `waitpid` + `getrusage(RUSAGE_CHILDREN)`: RUSAGE_CHILDREN is a running
/// maximum across *every* child ever reaped by this process, so at density each task would inherit
/// the largest peak of all prior tasks and the per-task figure would be meaningless. wait4 returns
/// the rusage of the one child just reaped. (`waitpid` + `/proc/self/status` VmHWM is worse still:
/// that reports the *agent's* footprint, which does not move when a child allocates.)
///
/// `std::process::Command` would be shorter but allocates, builds environment arrays, and sets up
/// pipes, so a timed region around it would include std's bookkeeping alongside the syscalls
/// under test.
///
/// Returns 128+signal for a child killed by a signal, matching shell convention. A test suite
/// killed by SIGKILL (the guest OOM killer, for instance) must not be reported as a pass.
#[cfg(target_os = "linux")]
fn spawn_and_wait(path: &str, argv: &[&str], cwd: Option<&str>) -> Result<i32, String> {
    spawn_and_wait_full(path, argv, cwd).map(|o| o.exit_code)
}

#[cfg(target_os = "linux")]
fn spawn_and_wait_full(
    path: &str,
    argv: &[&str],
    cwd: Option<&str>,
) -> Result<ChildOutcome, String> {
    use std::ffi::CString;

    // Every allocation happens before the fork. Between fork and execve the child may only make
    // async-signal-safe calls, and allocating there can deadlock on the allocator's lock.
    let c_path = CString::new(path).map_err(|e| e.to_string())?;
    let c_args: Vec<CString> = argv
        .iter()
        .map(|a| CString::new(*a).map_err(|e| e.to_string()))
        .collect::<Result<_, _>>()?;
    let mut ptrs: Vec<*const libc::c_char> = c_args.iter().map(|a| a.as_ptr()).collect();
    ptrs.push(std::ptr::null());
    // Allocated here rather than in the child for the same reason as everything else above.
    let c_cwd = cwd
        .map(|d| CString::new(d).map_err(|e| e.to_string()))
        .transpose()?;

    let pid = unsafe { libc::fork() };
    if pid < 0 {
        return Err(format!("fork: errno {}", errno()));
    }
    if pid == 0 {
        unsafe {
            // Working directory before exec, because a test runner is generally only correct when
            // invoked from its repository root: sympy's bin/test resolves test paths relative to
            // the cwd, and pytest's rootdir and conftest discovery both key off it. A distinct
            // exit code keeps a bad workdir from being read as a failed exec, which matters
            // because both would otherwise surface as an opaque non-zero reward.
            if let Some(dir) = &c_cwd {
                if libc::chdir(dir.as_ptr()) != 0 {
                    libc::_exit(126);
                }
            }
            libc::execv(c_path.as_ptr(), ptrs.as_ptr());
            // execv only returns on failure. _exit rather than exit: the child must not run
            // atexit handlers or flush the parent's buffers it inherited.
            libc::_exit(127);
        }
    }

    let mut status: libc::c_int = 0;
    // SAFETY: wait4 writes status and rusage into caller-provided storage. The zeroed rusage is a
    // valid initial value, and both are read only after the return is checked.
    let mut ru: libc::rusage = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::wait4(pid, &mut status, 0, &mut ru) };
    if rc < 0 {
        return Err(format!("wait4: errno {}", errno()));
    }
    Ok(ChildOutcome {
        exit_code: exit_code_from_status(status),
        // ru_maxrss is in kilobytes on Linux. Clamp because the field is signed.
        peak_rss_kib: ru.ru_maxrss.max(0) as u64,
    })
}

#[cfg(not(target_os = "linux"))]
fn spawn_child() -> Result<(), String> {
    // Non-Linux builds exist only to run the logic tests; the real agent is always Linux.
    Err("fork/exec is only implemented on Linux".to_string())
}

#[cfg(not(target_os = "linux"))]
fn spawn_and_wait(_path: &str, _argv: &[&str], _cwd: Option<&str>) -> Result<i32, String> {
    Err("fork/exec is only implemented on Linux".to_string())
}

#[cfg(not(target_os = "linux"))]
fn spawn_and_wait_full(
    _path: &str,
    _argv: &[&str],
    _cwd: Option<&str>,
) -> Result<ChildOutcome, String> {
    Err("fork/exec is only implemented on Linux".to_string())
}

/// Decode a waitpid status into an exit code.
///
/// Kept separate from the syscall wrapper so it is testable on any platform. The signal case is
/// not a corner case here: a test suite killed by the guest OOM killer exits via SIGKILL, and
/// reporting that as 0 would record a failed task as a passing one, inverting the reward.
fn exit_code_from_status(status: libc::c_int) -> i32 {
    // WIFEXITED / WEXITSTATUS / WTERMSIG are macros, so libc does not expose them. The bit
    // layout is stable on Linux: low 7 bits hold the terminating signal, next 8 the exit code.
    let sig = status & 0x7f;
    if sig == 0 {
        (status >> 8) & 0xff
    } else {
        128 + sig
    }
}

/// Write `write_kib` to a file in WORK_DIR, then remove it.
///
/// Removed each step so the tmpfs footprint stays bounded at roughly one step's output rather
/// than growing to `steps * write_kib`. Without that, a long episode would slowly consume guest
/// memory through tmpfs and eventually OOM, which would look like a density limit while
/// actually being an artifact of the harness.
fn write_step(step: u64, write_kib: u64) -> Result<(), String> {
    use std::io::Write as _;

    let path = format!("{WORK_DIR}/step-{step}.dat");
    let chunk = vec![0xa5u8; 4096];
    let mut f = std::fs::File::create(&path).map_err(|e| format!("create {path}: {e}"))?;
    let mut remaining = (write_kib as usize).saturating_mul(1024);
    while remaining > 0 {
        let n = remaining.min(chunk.len());
        f.write_all(&chunk[..n])
            .map_err(|e| format!("write {path}: {e}"))?;
        remaining -= n;
    }
    // Flush to the filesystem. On tmpfs this does not reach storage (there is none), but it
    // does force the pages to be allocated, which is the cost being measured.
    f.flush().map_err(|e| format!("flush {path}: {e}"))?;
    drop(f);
    std::fs::remove_file(&path).map_err(|e| format!("remove {path}: {e}"))?;
    Ok(())
}

/// File the image conversion writes with the task's command and metadata.
///
/// Baked into the rootfs by `docker-to-rootfs.sh` from the container image's `CMD`/`ENV`, which
/// `docker export` discards. Living in the image rather than arriving over vsock keeps the
/// protocol fixed-arity and means the image is self-describing: a converted rootfs can be booted
/// by hand and asked what it runs.
const TASK_ENV_PATH: &str = "/etc/fc-task.env";

#[derive(Debug)]
struct TaskResult {
    /// The child's exit code. **This is the RLVR reward.** Zero means the instance's
    /// fail-to-pass tests passed.
    exit_code: i32,
    /// Wall clock for the whole request, including reading the env file.
    total: std::time::Duration,
    /// Time before the child started: env file read and argv construction. Reported separately
    /// so the task's own duration can be separated from harness overhead.
    setup: std::time::Duration,
    /// Peak resident set of the task's own process, from `wait4`'s `ru_maxrss` -- not `VmHWM`,
    /// which this comment claimed until 2026-09-18 and which would report the agent's footprint
    /// instead. See the note on `spawn_and_wait_full` for why the child's rusage is the right
    /// source here and the agent's own peak is the right source for the sandbox.
    peak_rss_kib: u64,
}

/// Run the task baked into this image and report its exit status.
///
/// # Why the exit code is the point
///
/// A SWE-bench instance carries fail-to-pass tests: tests that fail on the unpatched repository
/// and pass on the reference solution. The test runner's exit status is therefore a
/// machine-checkable reward, with no reward model and no judge. That is what makes this an RLVR
/// measurement rather than a benchmark run, and it is why the exit code is reported before any
/// timing field: a fast task that crashed is not a result.
///
/// # What is deliberately not here
///
/// Child output is not captured. Piping it would need a reader thread to avoid filling the pipe
/// buffer and deadlocking a chatty test suite, and the byte count would not be worth that. The
/// child inherits the agent's stdout, which the guest kernel command line already points at a
/// disabled serial device (`8250.nr_uarts=0`) so it cannot consume host memory.
///
/// No retry, no timeout, no output parsing. A hung test suite is the host's problem to bound via
/// its request timeout, because only the host knows how long the cell is allowed to take. Parsing
/// pytest output to count individual tests would couple the agent to a specific runner; the exit
/// code is the contract every runner already honours.
fn run_task() -> Result<TaskResult, String> {
    run_task_from(TASK_ENV_PATH)
}

/// The body of [`run_task`], with the env file path injected.
///
/// Split out solely so the success path is testable. With the path hardcoded, the only reachable
/// test was the missing-file error, which left the exit-code propagation unexercised: a mutation
/// hardcoding the reward to 0 survived the whole suite. That is the one defect in this file that
/// must not ship, since it would report every task as a pass.
fn run_task_from(env_path: &str) -> Result<TaskResult, String> {
    let started = Instant::now();

    let env = std::fs::read_to_string(env_path)
        .map_err(|e| format!("read {env_path}: {e}. Was this image built by docker-to-rootfs?"))?;
    let cmd =
        parse_task_command(&env).ok_or_else(|| format!("{env_path} has no FC_TASK_CMD line"))?;
    if cmd.is_empty() {
        return Err(format!("{env_path} FC_TASK_CMD is empty"));
    }

    // The image's WORKDIR. A test runner is usually only correct from the repository root, so this
    // is part of the task definition rather than a convenience: SWE-bench images set it to
    // /testbed and their runners resolve test paths relative to it.
    let workdir = parse_task_field(&env, "FC_TASK_WORKDIR");

    let argv: Vec<&str> = cmd.iter().map(String::as_str).collect();
    let setup = started.elapsed();

    // wait4 gives back the child's own exit code and peak RSS together, so the reward and the
    // memory figure describe the same run and neither is polluted by a prior task's.
    let outcome = spawn_and_wait_full(argv[0], &argv, workdir.as_deref())?;

    Ok(TaskResult {
        exit_code: outcome.exit_code,
        total: started.elapsed(),
        setup,
        peak_rss_kib: outcome.peak_rss_kib,
    })
}

/// Key in the task env file naming the episode script, written by traj-to-episode.py.
const EPISODE_SCRIPT_KEY: &str = "FC_EPISODE_SCRIPT";

/// Cap on captured episode output.
///
/// An episode's turns can print a great deal -- a test suite alone emits megabytes -- and the guest
/// has no swap. Only the marker lines are needed, so output beyond this is counted and dropped
/// rather than retained.
const EPISODE_OUTPUT_CAP: usize = 256 * 1024;

/// A captured episode run: exit status, peak RSS, and the turn/verify time split.
struct CapturedRun {
    exit_code: i32,
    peak_rss_kib: u64,
    stdout: String,
    turns_us: u128,
    verify_us: u128,
}

/// Run a script, capturing stdout, and split the timing at the verifier marker.
///
/// This is the one place the agent uses a pipe. Every other workload deliberately avoids one -- the
/// child inherits stdout, which the kernel command line points at a disabled serial device -- but
/// the turn/verify split has to come from the script's own markers, and reading them needs the
/// output. The read loop drains continuously so a chatty test suite cannot fill the pipe buffer and
/// deadlock, which is the failure mode that made capture not worth it for the other verbs.
#[cfg(target_os = "linux")]
fn spawn_and_capture(path: &str, argv: &[&str]) -> Result<CapturedRun, String> {
    use std::ffi::CString;
    use std::io::Read;
    use std::os::fd::FromRawFd;

    let c_path = CString::new(path).map_err(|e| e.to_string())?;
    let c_args: Vec<CString> = argv
        .iter()
        .map(|a| CString::new(*a).map_err(|e| e.to_string()))
        .collect::<Result<_, _>>()?;
    let mut ptrs: Vec<*const libc::c_char> = c_args.iter().map(|a| a.as_ptr()).collect();
    ptrs.push(std::ptr::null());

    let mut fds = [0i32; 2];
    // SAFETY: pipe writes two descriptors into the provided array; the return is checked.
    if unsafe { libc::pipe(fds.as_mut_ptr()) } != 0 {
        return Err(format!("pipe: errno {}", errno()));
    }
    let (read_fd, write_fd) = (fds[0], fds[1]);

    let started = Instant::now();
    let pid = unsafe { libc::fork() };
    if pid < 0 {
        unsafe {
            libc::close(read_fd);
            libc::close(write_fd);
        }
        return Err(format!("fork: errno {}", errno()));
    }
    if pid == 0 {
        unsafe {
            // Child: stdout and stderr to the pipe, then exec. Only async-signal-safe calls here.
            libc::close(read_fd);
            libc::dup2(write_fd, 1);
            libc::dup2(write_fd, 2);
            libc::close(write_fd);
            libc::execv(c_path.as_ptr(), ptrs.as_ptr());
            libc::_exit(127);
        }
    }

    // Parent: close the write end, or the read below never sees EOF.
    unsafe { libc::close(write_fd) };
    // SAFETY: read_fd is owned by this process and not used elsewhere after this point.
    let mut reader = unsafe { std::fs::File::from_raw_fd(read_fd) };

    let mut captured = String::new();
    let mut dropped = 0usize;
    let mut buf = [0u8; 8192];
    // The instant the verifier marker appears, which is what separates turn time from verify time.
    let mut verify_start: Option<Instant> = None;
    loop {
        match reader.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => {
                let chunk = String::from_utf8_lossy(&buf[..n]);
                if verify_start.is_none() && chunk.contains("EPISODE_VERIFY_START") {
                    verify_start = Some(Instant::now());
                }
                if captured.len() < EPISODE_OUTPUT_CAP {
                    captured.push_str(&chunk);
                } else {
                    dropped += n;
                }
            }
            Err(ref e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(format!("read episode output: {e}")),
        }
    }
    if dropped > 0 {
        // Recorded rather than silent: a reader comparing captured output against a full log should
        // know it was truncated.
        captured.push_str(&format!(
            "\n[{dropped} bytes of output dropped past the cap]\n"
        ));
    }

    let mut status: libc::c_int = 0;
    let mut ru: libc::rusage = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::wait4(pid, &mut status, 0, &mut ru) };
    if rc < 0 {
        return Err(format!("wait4: errno {}", errno()));
    }
    let elapsed = started.elapsed();

    // If the marker never appeared the episode did not reach its verifier, so all of the time was
    // turns and none of it was verification. Reporting the total as verify time would invert the
    // ratio this measurement exists to establish.
    let (turns_us, verify_us) = match verify_start {
        Some(t) => {
            let verify = t.elapsed();
            (
                elapsed.saturating_sub(verify).as_micros(),
                verify.as_micros(),
            )
        }
        None => (elapsed.as_micros(), 0),
    };

    Ok(CapturedRun {
        exit_code: exit_code_from_status(status),
        peak_rss_kib: ru.ru_maxrss.max(0) as u64,
        stdout: captured,
        turns_us,
        verify_us,
    })
}

#[cfg(not(target_os = "linux"))]
fn spawn_and_capture(_path: &str, _argv: &[&str]) -> Result<CapturedRun, String> {
    Err("fork/exec is only implemented on Linux".to_string())
}

#[derive(Debug)]
struct EpisodeResult {
    /// Turns the episode ran, as counted by the script itself.
    turns: u64,
    /// Turns that exited zero. Below `turns` is normal and expected: a real rollout contains
    /// mistakes, and the recorded trajectory this replays has an edit rejected for bad indentation
    /// before the retry succeeds. Reported so the host can tell a rollout's own failures from an
    /// episode whose turns stopped executing.
    turns_ok: u64,
    /// Wall clock for the whole request.
    total: std::time::Duration,
    /// Microseconds spent on the agent's turns, excluding the verifier.
    turns_us: u128,
    /// Microseconds spent in the verifier alone.
    ///
    /// Split from `turns_us` because the ratio between them is the entire point of replaying an
    /// episode: every earlier measurement in this project timed a verifier run and called it an
    /// episode. One combined number would hide exactly the quantity being established.
    verify_us: u128,
    /// The verifier's exit status, which is the reward.
    exit_code: i32,
    /// Peak resident set of the episode process tree.
    peak_rss_kib: u64,
}

/// Replay a recorded agent trajectory as a full RL episode.
///
/// # What this measures that `RUN_TASK` does not
///
/// `RUN_TASK` runs a verifier once. An RL episode is tens of turns -- the policy proposes a command,
/// the sandbox runs it -- and only then does the verifier decide the reward. The shapes differ: an
/// episode is many short process spawns and repeated file reads over a warm page cache, while a
/// verifier run is one long compute-bound process. Sizing a fleet from the latter and calling it the
/// former understates an episode by however many turns it has.
///
/// # How the split is obtained
///
/// The script emits `EPISODE_TURNS=`, `EPISODE_TURNS_OK=` and `EPISODE_VERIFY_START` markers on
/// stdout. The agent reads them rather than timing turns itself, because the alternative -- parsing
/// the script and spawning each turn from here -- would put the agent in the business of
/// understanding shell, and the script already knows where its own boundaries are.
fn run_episode() -> Result<EpisodeResult, String> {
    run_episode_from(TASK_ENV_PATH)
}

/// The body of [`run_episode`], with the env file path injected so the success path is testable.
/// The same reasoning as `run_task_from`: with the path hardcoded, only the missing-file error was
/// reachable and a mutation hardcoding the reward survived the suite.
fn run_episode_from(env_path: &str) -> Result<EpisodeResult, String> {
    let started = Instant::now();

    let env = std::fs::read_to_string(env_path)
        .map_err(|e| format!("read {env_path}: {e}. Was this image built by traj-to-episode?"))?;
    let script = parse_task_field(&env, EPISODE_SCRIPT_KEY)
        .ok_or_else(|| format!("{env_path} has no {EPISODE_SCRIPT_KEY} line"))?;

    // The interpreter is resolved at conversion time and stored with the script, because execv does
    // not search PATH: a bare "bash" exits 127 with bash present in the image, which cost a gate
    // failure on hardware once already.
    // Whitespace-split so an interpreter needing an applet argument works -- busybox dispatches on
    // "busybox sh" -- and so the first word is the path execv receives.
    let shell_spec =
        parse_task_field(&env, "FC_EPISODE_SHELL").unwrap_or_else(|| "/bin/bash".to_string());
    let mut argv: Vec<&str> = shell_spec.split_whitespace().collect();
    let shell = *argv
        .first()
        .ok_or_else(|| format!("{env_path} FC_EPISODE_SHELL is empty"))?;
    argv.push(&script);

    let outcome = spawn_and_capture(shell, &argv)?;

    // Markers, read from the script's own accounting. Absent markers mean the script did not reach
    // its end, which is a broken episode rather than a fast one -- so they are an error, not a zero.
    let turns = marker_u64(&outcome.stdout, "EPISODE_TURNS=")
        .ok_or("episode did not report EPISODE_TURNS; it exited before finishing its turns")?;
    let turns_ok = marker_u64(&outcome.stdout, "EPISODE_TURNS_OK=")
        .ok_or("episode did not report EPISODE_TURNS_OK")?;

    Ok(EpisodeResult {
        turns,
        turns_ok,
        total: started.elapsed(),
        turns_us: outcome.turns_us,
        verify_us: outcome.verify_us,
        exit_code: outcome.exit_code,
        peak_rss_kib: outcome.peak_rss_kib,
    })
}

/// Read `<key><integer>` from captured output.
fn marker_u64(text: &str, key: &str) -> Option<u64> {
    for line in text.lines() {
        if let Some(rest) = line.trim().strip_prefix(key) {
            return rest.trim().parse().ok();
        }
    }
    None
}

/// Extract `FC_TASK_CMD` from the env file as an argv vector.
///
/// Format is one `KEY=value` per line. `FC_TASK_CMD` is whitespace-separated argv, which is
/// enough for the SWE-bench case (`python -m pytest <test>`) and avoids putting a shell in the
/// image. A command needing shell features should be written as
/// `FC_TASK_CMD=/bin/sh -c "..."` at conversion time, where the quoting is handled once.
fn parse_task_command(env: &str) -> Option<Vec<String>> {
    let raw = parse_task_field(env, "FC_TASK_CMD")?;
    Some(raw.split_whitespace().map(str::to_string).collect())
}

/// Read one `KEY=value` line from the task env file.
///
/// Shared by the command and working-directory reads so quote handling and comment skipping exist
/// once. Returns `None` for a missing or empty value, which lets callers treat "unset" and "set to
/// nothing" alike -- an empty workdir should mean "do not chdir", not "chdir to the empty string".
fn parse_task_field(env: &str, key: &str) -> Option<String> {
    let prefix = format!("{key}=");
    for line in env.lines() {
        let line = line.trim();
        if line.starts_with('#') {
            continue;
        }
        if let Some(rest) = line.strip_prefix(&prefix) {
            // Strip one layer of surrounding quotes if present, since a writer may add them.
            let rest = rest.trim();
            let rest = rest
                .strip_prefix('"')
                .and_then(|r| r.strip_suffix('"'))
                .unwrap_or(rest);
            if rest.is_empty() {
                return None;
            }
            return Some(rest.to_string());
        }
    }
    None
}

/// Build a permutation of `0..entries` that forms a single cycle of full length.
///
/// Sattolo's algorithm, not Fisher-Yates. The distinction is the whole measurement: Fisher-Yates
/// produces a permutation with several cycles, so a chase starting anywhere can land in a short
/// one, revisit a handful of entries, and stay resident in L1 no matter how large the buffer is.
/// The workload would then look cache-insensitive for a reason that has nothing to do with
/// placement. The bound `j < i` rather than `j <= i` is what guarantees the single cycle.
///
/// Extracted so the tests exercise this exact code rather than a copy of it. A duplicated
/// shuffle in the test cannot detect a change to the real one.
fn build_cycle(entries: usize) -> Vec<u32> {
    let mut ring: Vec<u32> = (0..entries as u32).collect();
    let mut rng = SplitMix64::new(0x243f_6a88_85a3_08d3);
    for i in (1..entries).rev() {
        let j = (rng.next() % i as u64) as usize; // strictly less than i, never equal
        ring.swap(i, j);
    }
    ring
}

/// SplitMix64, so the permutation is deterministic and dependency-free.
///
/// Determinism matters: every microVM must chase the same access pattern, or a latency
/// difference between them could be the pattern rather than the placement.
struct SplitMix64 {
    state: u64,
}

impl SplitMix64 {
    fn new(seed: u64) -> Self {
        Self { state: seed }
    }
    fn next(&mut self) -> u64 {
        self.state = self.state.wrapping_add(0x9e37_79b9_7f4a_7c15);
        let mut z = self.state;
        z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
        z ^ (z >> 31)
    }
}

/// A compute loop standing in for RL policy inference.
///
/// Deliberately arithmetic-bound with a data dependency between iterations, so it cannot be
/// vectorised or reordered into something that finishes early. `black_box` and the returned
/// checksum together stop the optimiser from removing work the measurement depends on.
#[inline(never)]
fn compute_loop(iterations: u64) -> u64 {
    let mut acc: u64 = 0x9e37_79b9_7f4a_7c15;
    for i in 0..iterations {
        // Each step depends on the previous, which prevents instruction-level parallelism
        // from hiding the cost and makes the loop a reasonable proxy for serial work.
        acc = acc.wrapping_mul(6364136223846793005).wrapping_add(i | 1);
        acc ^= acc >> 33;
        std::hint::black_box(acc);
    }
    acc
}

/// Mount /proc and /sys. Only meaningful when running as PID 1.
///
/// Gated to Linux because `mount(2)`'s signature is Linux-specific. The gate also lets the
/// pure-logic tests compile and run on a developer's macOS machine; the target build is
/// always Linux/musl.
#[cfg(target_os = "linux")]
fn mount_pseudo_filesystems() {
    // Failures are logged rather than fatal: a guest without /proc is degraded but can still
    // answer requests, and panicking here would turn a minor problem into a dead microVM.
    // /tmp is a tmpfs because the rootfs is mounted read-only so that one page-cache copy can
    // serve the whole fleet. Without it, any workload that writes fails with EROFS.
    //
    // No size= option, so the kernel default of half of RAM applies, which scales with the
    // guest's configured memory rather than needing to be threaded through from the host. Note
    // that **tmpfs pages are guest memory**: a workload writing here consumes the same budget
    // as its heap. That is a real property of a read-only-rootfs sandbox design, not an
    // artifact of this harness, and it is why the sandbox workload takes write size as a
    // separate parameter from resident size.
    for (target, fstype) in [("/proc", "proc"), ("/sys", "sysfs"), ("/tmp", "tmpfs")] {
        let tgt = std::ffi::CString::new(target).unwrap();
        let fs = std::ffi::CString::new(fstype).unwrap();
        let rc =
            unsafe { libc::mount(fs.as_ptr(), tgt.as_ptr(), fs.as_ptr(), 0, std::ptr::null()) };
        if rc != 0 {
            eprintln!("fc-guest-agent: mount {target} failed (errno {})", errno());
        }
    }
}

#[cfg(not(target_os = "linux"))]
fn mount_pseudo_filesystems() {
    // Non-Linux builds exist only to run the logic tests. The real agent always runs on
    // Linux as PID 1.
}

/// Reap exited children.
///
/// As PID 1 the agent inherits orphans. Without reaping, zombies accumulate and consume
/// PIDs, which at density is a slow leak across the whole fleet rather than one guest's
/// problem.
fn reap_children() {
    loop {
        let rc = unsafe { libc::waitpid(-1, std::ptr::null_mut(), libc::WNOHANG) };
        if rc <= 0 {
            break;
        }
    }
}

fn errno() -> i32 {
    std::io::Error::last_os_error().raw_os_error().unwrap_or(0)
}

// ---- Minimal AF_VSOCK support ----
//
// std has no vsock type, and pulling in a crate for it would add dependency weight to a
// binary that is multiplied across the fleet. The socket calls are short enough to make
// directly.

struct VsockListener {
    fd: OwnedFd,
}

impl VsockListener {
    fn bind(port: u32) -> std::io::Result<Self> {
        let fd = unsafe { libc::socket(AF_VSOCK, libc::SOCK_STREAM, 0) };
        if fd < 0 {
            return Err(std::io::Error::last_os_error());
        }
        let fd = unsafe { OwnedFd::from_raw_fd(fd) };

        let addr = SockAddrVm::new(VMADDR_CID_ANY, port);
        let rc = unsafe {
            libc::bind(
                fd.as_raw_fd(),
                &addr as *const SockAddrVm as *const libc::sockaddr,
                std::mem::size_of::<SockAddrVm>() as libc::socklen_t,
            )
        };
        if rc != 0 {
            return Err(std::io::Error::last_os_error());
        }

        // A modest backlog is enough: the host opens one connection per request rather than
        // many concurrent ones per guest.
        if unsafe { libc::listen(fd.as_raw_fd(), 8) } != 0 {
            return Err(std::io::Error::last_os_error());
        }

        Ok(Self { fd })
    }

    fn accept(&self) -> std::io::Result<VsockStream> {
        let fd = unsafe {
            libc::accept(
                self.fd.as_raw_fd(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
            )
        };
        if fd < 0 {
            return Err(std::io::Error::last_os_error());
        }
        Ok(VsockStream {
            fd: unsafe { OwnedFd::from_raw_fd(fd) },
        })
    }
}

struct VsockStream {
    fd: OwnedFd,
}

impl VsockStream {
    fn try_clone(&self) -> std::io::Result<Self> {
        Ok(Self {
            fd: self.fd.try_clone()?,
        })
    }
}

impl std::io::Read for VsockStream {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let n = unsafe {
            libc::read(
                self.fd.as_raw_fd(),
                buf.as_mut_ptr() as *mut libc::c_void,
                buf.len(),
            )
        };
        if n < 0 {
            return Err(std::io::Error::last_os_error());
        }
        Ok(n as usize)
    }
}

impl Write for VsockStream {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        let n = unsafe {
            libc::write(
                self.fd.as_raw_fd(),
                buf.as_ptr() as *const libc::c_void,
                buf.len(),
            )
        };
        if n < 0 {
            return Err(std::io::Error::last_os_error());
        }
        Ok(n as usize)
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(()) // unbuffered
    }
}

/// `struct sockaddr_vm` from the kernel.
#[repr(C)]
struct SockAddrVm {
    svm_family: libc::sa_family_t,
    svm_reserved1: u16,
    svm_port: u32,
    svm_cid: u32,
    svm_zero: [u8; 4],
}

impl SockAddrVm {
    fn new(cid: u32, port: u32) -> Self {
        Self {
            svm_family: AF_VSOCK as libc::sa_family_t,
            svm_reserved1: 0,
            svm_port: port,
            svm_cid: cid,
            svm_zero: [0; 4],
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_ping() {
        assert!(matches!(parse_request("PING"), Some(Request::Ping)));
    }

    #[test]
    fn parses_run_with_iterations() {
        match parse_request("RUN 1000") {
            Some(Request::Run { iterations }) => assert_eq!(iterations, 1000),
            other => panic!("expected Run, got {:?}", other.is_some()),
        }
    }

    #[test]
    fn rejects_unknown_and_malformed() {
        assert!(parse_request("").is_none());
        assert!(parse_request("NONSENSE").is_none());
        assert!(
            parse_request("RUN").is_none(),
            "RUN without a count must be rejected"
        );
        assert!(
            parse_request("RUN abc").is_none(),
            "non-numeric count must be rejected"
        );
    }

    #[test]
    fn tolerates_extra_whitespace() {
        // The host writes a line; being lenient about spacing avoids a class of confusing
        // failures that would look like a hung guest.
        match parse_request("  RUN   500  ") {
            Some(Request::Run { iterations }) => assert_eq!(iterations, 500),
            _ => panic!("expected Run"),
        }
    }

    /// The loop must not be optimised away, or the measurement would report an
    /// implausibly fast host rather than the cost of real work.
    #[test]
    fn compute_loop_is_not_elided() {
        let a = compute_loop(1_000);
        let b = compute_loop(2_000);
        assert_ne!(
            a, b,
            "different iteration counts must produce different checksums"
        );

        let start = Instant::now();
        compute_loop(50_000_000);
        let elapsed = start.elapsed();
        assert!(
            elapsed.as_micros() > 100,
            "50M iterations completed in {elapsed:?}, which suggests the loop was elided"
        );
    }

    #[test]
    fn compute_loop_is_deterministic() {
        // Determinism matters: a varying checksum for the same input would make it useless
        // as a check that the work ran.
        assert_eq!(compute_loop(10_000), compute_loop(10_000));
    }

    #[test]
    fn zero_iterations_is_safe() {
        // The host could legitimately send RUN 0, and it must not divide by zero.
        let _ = compute_loop(0);
    }

    #[test]
    fn parses_run_cache() {
        match parse_request("RUN_CACHE 4096 100000") {
            Some(Request::RunCache {
                working_set_kib,
                iterations,
            }) => {
                assert_eq!(working_set_kib, 4096);
                assert_eq!(iterations, 100_000);
            }
            _ => panic!("expected RunCache"),
        }
    }

    #[test]
    fn rejects_malformed_run_cache() {
        for bad in [
            "RUN_CACHE",          // no arguments
            "RUN_CACHE 4096",     // missing iterations
            "RUN_CACHE abc 1000", // non-numeric working set
            "RUN_CACHE 4096 abc", // non-numeric iterations
        ] {
            assert!(
                parse_request(bad).is_none(),
                "parse_request({bad:?}) should have been rejected"
            );
        }
    }

    /// A working set larger than the guest's RAM must be refused, not attempted. An OOM kill
    /// presents to the host as a microVM that stopped answering, which is far harder to
    /// diagnose than an error string.
    #[test]
    fn run_cache_refuses_oversized_and_degenerate_working_sets() {
        assert!(run_cache(MAX_WORKING_SET_KIB + 1, 10).is_err());
        assert!(run_cache(0, 10).is_err());
        // At the limit it must still be accepted, or the boundary is off by one.
        assert!(run_cache(MAX_WORKING_SET_KIB, 10).is_ok());
    }

    /// The permutation must be ONE cycle covering every entry.
    ///
    /// This is the property the whole measurement rests on. A Fisher-Yates shuffle would give
    /// several shorter cycles, and a chase could then revisit a handful of entries that stay in
    /// L1 no matter how large the buffer is. The workload would look cache-insensitive and the
    /// CCX-affinity experiment would report "no effect" for a reason that has nothing to do with
    /// placement. Sattolo's algorithm is what guarantees the single cycle.
    #[test]
    fn permutation_is_a_single_full_cycle() {
        // Rebuild the ring exactly as run_cache does, then walk it.
        for &entries in &[64usize, 1024, 4096, 65536] {
            let ring = build_cycle(entries);

            // Count cycles over the WHOLE permutation rather than walking from index 0.
            //
            // Walking from one start only proves that start's cycle is full when it happens to
            // be the long one. A Fisher-Yates shuffle typically leaves one large cycle plus
            // several small ones, so a walk from 0 can pass while short cycles exist elsewhere
            // -- verified by mutating Sattolo to Fisher-Yates, which a start-from-0 version of
            // this test failed to catch. Counting every cycle is what makes the mutant die.
            let mut visited = vec![false; entries];
            let mut cycles = 0usize;
            let mut fixed_points = 0usize;
            for start in 0..entries {
                if visited[start] {
                    continue;
                }
                cycles += 1;
                let mut len = 0usize;
                let mut idx = start;
                while !visited[idx] {
                    visited[idx] = true;
                    idx = ring[idx] as usize;
                    len += 1;
                }
                if len == 1 {
                    fixed_points += 1;
                }
            }
            assert_eq!(
                cycles, 1,
                "entries={entries}: permutation has {cycles} cycles, want exactly 1. \
                 Multiple cycles let the chase revisit a small subset that stays resident in \
                 L1 regardless of buffer size, which silently makes the workload \
                 cache-insensitive."
            );
            assert_eq!(
                fixed_points, 0,
                "entries={entries}: {fixed_points} index maps to itself, which would spin on \
                 one cache line"
            );
            assert!(
                visited.iter().all(|v| *v),
                "entries={entries}: some indices were never visited"
            );
        }
    }

    /// Every entry must appear exactly once, or the chase would visit a value out of range or
    /// skip part of the buffer.
    #[test]
    fn permutation_is_a_bijection() {
        let entries = 2048usize;
        let ring = build_cycle(entries);
        let mut sorted = ring.clone();
        sorted.sort_unstable();
        assert_eq!(
            sorted,
            (0..entries as u32).collect::<Vec<_>>(),
            "the permutation is not a bijection over 0..entries"
        );
    }

    /// The point of the whole exercise: per-iteration latency must rise as the working set
    /// outgrows the caches. If this does not hold, the workload is not cache-sensitive and
    /// cannot evaluate cache-affinity placement.
    ///
    /// The assertion is deliberately loose. Exact ratios are hardware-specific, and a tight
    /// bound here would make the test fail on a developer machine while telling us nothing.
    /// What matters is the direction and that the effect is not negligible.
    #[test]
    fn larger_working_set_is_slower_per_access() {
        const ITERS: u64 = 2_000_000;
        // 16 KiB fits L1 on any modern core. 32 MiB exceeds L2 everywhere and most L3 shares.
        let small = run_cache(16, ITERS).expect("small working set");
        let large = run_cache(32 * 1024, ITERS).expect("large working set");

        assert!(
            large.mean_ns > small.mean_ns,
            "16 KiB took {} ns/access and 32 MiB took {} ns/access; the pointer chase is not \
             cache-sensitive, so it cannot measure cache affinity",
            small.mean_ns,
            large.mean_ns
        );
        // A real L1-to-DRAM gap is more than an order of magnitude. Requiring only 2x leaves
        // room for a loaded CI machine while still failing if the effect has vanished.
        assert!(
            large.mean_ns >= small.mean_ns * 2,
            "expected at least 2x slower for a 2048x larger working set, got {} vs {} ns/access",
            large.mean_ns,
            small.mean_ns
        );
    }

    /// Setup must be excluded from the timed region. Allocating and shuffling 32 MiB takes
    /// longer than the chase itself at low iteration counts, so including it would swamp the
    /// measurement.
    #[test]
    fn setup_is_measured_separately_from_the_chase() {
        let r = run_cache(16 * 1024, 1_000).expect("run");
        assert!(
            r.setup.as_nanos() > 0,
            "setup time was not measured, so there is no evidence it is outside the chase"
        );
        // With only 1000 iterations over a 16 MiB buffer, setup should dominate. If chase were
        // larger, setup work would be leaking into the timed region.
        assert!(
            r.setup > r.chase,
            "setup {:?} should exceed chase {:?} at 1000 iterations over 16 MiB; if not, \
             setup cost is being counted as chase cost",
            r.setup,
            r.chase
        );
    }

    #[test]
    fn cache_chase_is_deterministic() {
        // Same inputs must give the same checksum, or it is useless as proof the work ran.
        let a = run_cache(256, 50_000).expect("a");
        let b = run_cache(256, 50_000).expect("b");
        assert_eq!(a.checksum, b.checksum);
    }

    #[test]
    fn cache_chase_is_not_elided() {
        // Different iteration counts must produce different checksums.
        let a = run_cache(256, 10_000).expect("a");
        let b = run_cache(256, 20_000).expect("b");
        assert_ne!(
            a.checksum, b.checksum,
            "identical checksums for different iteration counts suggest the chase was elided"
        );
    }

    #[test]
    fn cache_chase_zero_iterations_is_safe() {
        let r = run_cache(256, 0).expect("zero iterations must not error");
        assert_eq!(r.mean_ns, 0, "must not divide by zero");
    }

    #[test]
    fn parses_run_sandbox() {
        match parse_request("RUN_SANDBOX 2097152 20 512") {
            Some(Request::RunSandbox {
                resident_kib,
                steps,
                write_kib,
            }) => {
                assert_eq!(resident_kib, 2_097_152, "2 GiB in KiB");
                assert_eq!(steps, 20);
                assert_eq!(write_kib, 512);
            }
            _ => panic!("expected RunSandbox"),
        }
    }

    #[test]
    fn rejects_malformed_run_sandbox() {
        for bad in [
            "RUN_SANDBOX",            // no arguments
            "RUN_SANDBOX 1024",       // missing steps and write size
            "RUN_SANDBOX 1024 20",    // missing write size
            "RUN_SANDBOX abc 20 512", // non-numeric resident
            "RUN_SANDBOX 1024 x 512", // non-numeric steps
            "RUN_SANDBOX 1024 20 x",  // non-numeric write size
        ] {
            assert!(
                parse_request(bad).is_none(),
                "parse_request({bad:?}) should have been rejected"
            );
        }
    }

    /// A request beyond the guest's memory must be refused rather than attempted. An OOM kill
    /// presents to the host as a microVM that stopped answering, which is far harder to
    /// diagnose than an error string, and at density it would look like a spurious density
    /// ceiling.
    #[test]
    fn run_sandbox_refuses_oversized_resident_set() {
        let err = run_sandbox(MAX_RESIDENT_KIB + 1, 1, 0)
            .expect_err("a resident set beyond the limit must be refused");
        assert!(
            err.contains("exceeds"),
            "error {err:?} should explain the limit"
        );
    }

    /// The three workloads must be requested with distinct verbs and answered with distinct
    /// tags. Field positions differ between them, so a shared tag would let the host record a
    /// resident-set size as an iteration count.
    #[test]
    fn sandbox_reply_tag_is_distinct() {
        // OKS carries 8 fields; OK carries 5 and OKC carries 7. Verified here against the
        // formatting in handle() by checking the parse side rejects the wrong shapes.
        assert!(parse_request("RUN_SANDBOX 1024 5 64").is_some());
        assert!(parse_request("RUN_CACHE 1024 5").is_some());
        assert!(parse_request("RUN 5").is_some());
    }

    /// Zero steps must be safe: the resident set is still allocated and faulted in, which is a
    /// legitimate way to measure pure memory occupancy with no episode activity.
    #[test]
    fn run_sandbox_zero_steps_allocates_but_does_nothing() {
        let r = run_sandbox(1024, 0, 0).expect("zero steps must not error");
        assert_eq!(r.spawns_ok, 0);
        assert_eq!(r.spawn, std::time::Duration::ZERO);
        assert_eq!(r.write, std::time::Duration::ZERO);
    }

    /// The sandbox reply must carry a peak that reflects the *agent's* own footprint, because the
    /// resident buffer lives here and the children are trivial.
    ///
    /// Asserted against the buffer size rather than a fixed constant: a 64 MiB buffer must push
    /// VmHWM to at least 64 MiB, and a `wait4`-based implementation would report the few hundred
    /// KiB of `busybox true` instead and fail this. That is the inversion this measurement exists
    /// to get right -- `spawn_and_wait_full` is correct for RUN_TASK for the opposite reason.
    ///
    /// Linux-only: VmHWM comes from /proc/self/status, which macOS does not provide.
    #[cfg(target_os = "linux")]
    #[test]
    fn sandbox_peak_rss_reflects_the_agents_own_buffer() {
        const KIB: u64 = 64 * 1024; // 64 MiB
        let r = run_sandbox(KIB, 2, 0).expect("sandbox must run");
        assert!(
            r.peak_rss_kib >= KIB,
            "peak {} KiB is below the {} KiB resident buffer; a child's rusage was measured \
             instead of the agent's own VmHWM",
            r.peak_rss_kib,
            KIB
        );
    }

    /// A zero peak must mean "not measured", never "no memory used".
    ///
    /// This process has a footprint by definition, so a zero here would mean the VmHWM parse
    /// silently failed -- and a small wrong number is worse than an obvious one, because it would
    /// be published as a real per-microVM figure.
    #[cfg(target_os = "linux")]
    #[test]
    fn self_peak_rss_is_readable_and_nonzero() {
        assert!(
            self_peak_rss_kib() > 0,
            "VmHWM parsed as 0, which would be recorded as a real measurement"
        );
    }

    /// Every page of the resident set must be faulted in, not merely allocated.
    ///
    /// Asserted by reading the sentinel back from every page, **not** by timing. A timing-based
    /// version of this test passed even with the fault-in loop deleted, because `vec!` already
    /// touches pages on some platforms. Verified by mutation: deleting the loop, or striding by
    /// more than a page, both fail this.
    ///
    /// This matters because the host measures guest memory with `smaps_rollup`. Pages that are
    /// allocated but never touched do not appear there, so a non-resident buffer would make a
    /// 2 GiB guest look like a much smaller one and give a wrong answer to the memory-ceiling
    /// question.
    #[test]
    fn every_page_of_the_resident_set_is_touched() {
        const BYTES: usize = 1024 * 1024; // 1 MiB = 256 pages
        let buf = allocate_resident(BYTES);
        assert_eq!(buf.len(), BYTES);

        let pages = BYTES / PAGE_SIZE;
        let marked = (0..BYTES)
            .step_by(PAGE_SIZE)
            .filter(|&i| buf[i] == RESIDENT_MARK)
            .count();
        assert_eq!(
            marked, pages,
            "only {marked} of {pages} pages carry the residency mark; untouched pages do not \
             appear in smaps_rollup, so the host would under-measure guest memory"
        );
    }

    /// A zero-size resident set must not panic. Legitimate: it isolates fork/write cost from
    /// memory cost.
    #[test]
    fn resident_set_of_zero_is_safe() {
        assert!(allocate_resident(0).is_empty());
    }

    /// Sub-page allocations must still be handled. `step_by` on a range shorter than the stride
    /// yields exactly one index, so the first page is marked and nothing runs off the end.
    #[test]
    fn resident_set_smaller_than_a_page_is_safe() {
        let buf = allocate_resident(100);
        assert_eq!(buf.len(), 100);
        assert_eq!(buf[0], RESIDENT_MARK);
    }

    /// The resident set must actually be touched, or the "resident" figure the host measures
    /// would be fiction. A larger resident set with the same step count must take longer,
    /// because every page is revisited each step.
    ///
    /// Linux-only because any steps > 0 needs a working fork, which is stubbed out on other
    /// platforms. The non-Linux build exists only to run the pure-logic tests.
    #[cfg(target_os = "linux")]
    #[test]
    fn larger_resident_set_costs_more_to_maintain() {
        const STEPS: u64 = 4;
        let small = run_sandbox(256, STEPS, 0).expect("small");
        let large = run_sandbox(16 * 1024, STEPS, 0).expect("large");

        // Compare the non-spawn, non-write portion: that is the resident-set maintenance.
        let small_touch = small.total.saturating_sub(small.spawn + small.write);
        let large_touch = large.total.saturating_sub(large.spawn + large.write);
        assert!(
            large_touch > small_touch,
            "16 MiB resident took {large_touch:?} to maintain against 256 KiB at \
             {small_touch:?}; the resident set is not being touched"
        );
    }

    /// fork/exec must be timed separately from the rest of the episode, because process
    /// creation is the path an arithmetic loop never exercises and the one most likely to
    /// behave differently at density. If it were folded into the total, that could not be seen.
    #[cfg(target_os = "linux")]
    #[test]
    fn spawn_time_is_measured_and_nonzero() {
        let r = run_sandbox(0, 8, 0).expect("run");
        assert_eq!(r.spawns_ok, 8, "all 8 children should have been reaped");
        assert!(
            r.spawn > std::time::Duration::ZERO,
            "spawn time was not measured, so fork/exec cost cannot be separated"
        );
        assert!(
            r.spawn <= r.total,
            "spawn {:?} cannot exceed total {:?}",
            r.spawn,
            r.total
        );
    }

    /// Writes must be timed separately and must actually happen. Writing to a read-only rootfs
    /// would fail with EROFS, so this also confirms the workload targets a writable mount.
    #[cfg(target_os = "linux")]
    #[test]
    fn write_time_is_measured_when_requested() {
        let none = run_sandbox(0, 4, 0).expect("no writes");
        assert_eq!(
            none.write,
            std::time::Duration::ZERO,
            "write time should be zero when write_kib is 0"
        );

        let some = run_sandbox(0, 4, 64).expect("with writes");
        assert!(
            some.write > std::time::Duration::ZERO,
            "write time was not measured despite write_kib=64"
        );
    }

    /// Files must not accumulate across steps. Without per-step cleanup a long episode would
    /// grow the tmpfs until the guest OOMs, which would look like a density limit while
    /// actually being an artifact of the harness.
    #[cfg(target_os = "linux")]
    #[test]
    fn writes_do_not_accumulate() {
        let before = count_work_files();
        run_sandbox(0, 12, 32).expect("run");
        let after = count_work_files();
        assert_eq!(
            before, after,
            "step files were left behind: {before} before, {after} after"
        );
    }

    #[cfg(target_os = "linux")]
    fn count_work_files() -> usize {
        std::fs::read_dir(WORK_DIR)
            .map(|rd| {
                rd.filter_map(Result::ok)
                    .filter(|e| e.file_name().to_string_lossy().starts_with("step-"))
                    .count()
            })
            .unwrap_or(0)
    }

    #[test]
    fn parses_run_task() {
        assert!(matches!(parse_request("RUN_TASK"), Some(Request::RunTask)));
        // Trailing whitespace is normal from a line-oriented protocol.
        assert!(matches!(
            parse_request("  RUN_TASK  "),
            Some(Request::RunTask)
        ));
    }

    #[test]
    fn parses_run_episode() {
        assert!(matches!(
            parse_request("RUN_EPISODE"),
            Some(Request::RunEpisode)
        ));
        assert!(matches!(
            parse_request("  RUN_EPISODE  "),
            Some(Request::RunEpisode)
        ));
        // RUN_EPISODE and RUN_TASK must not be confusable: they report different reply tags with
        // different field layouts, and the host refuses a cell whose tag does not match what it
        // asked for.
        assert!(matches!(parse_request("RUN_TASK"), Some(Request::RunTask)));
    }

    #[test]
    fn reads_integer_markers_from_episode_output() {
        let out = "noise\nEPISODE_TURNS=14\nEPISODE_TURNS_OK=13\nmore noise\n";
        assert_eq!(marker_u64(out, "EPISODE_TURNS="), Some(14));
        assert_eq!(marker_u64(out, "EPISODE_TURNS_OK="), Some(13));
        // A missing marker is None, not zero: an episode that did not report its turns did not
        // finish, and reporting zero turns would look like a fast episode instead of a broken one.
        assert_eq!(marker_u64(out, "EPISODE_VERIFY_RC="), None);
        assert_eq!(marker_u64("", "EPISODE_TURNS="), None);
        // EPISODE_TURNS is a prefix of EPISODE_TURNS_OK, so a prefix match would read the wrong
        // line whenever the OK count came first.
        let reordered = "EPISODE_TURNS_OK=13\nEPISODE_TURNS=14\n";
        assert_eq!(marker_u64(reordered, "EPISODE_TURNS="), Some(14));
    }

    /// A missing env file must be a readable error, not a panic: as PID 1 a panic takes the guest
    /// down and the host sees a microVM that stopped answering rather than a misbuilt image.
    #[test]
    fn run_episode_without_env_file_errors_clearly() {
        let err = run_episode().expect_err("no env file should be an error");
        assert!(
            err.contains(TASK_ENV_PATH),
            "error should name the missing file, got: {err}"
        );
    }

    /// The turn/verify split must be real, and the turn count must come from the episode.
    ///
    /// This is the measurement the whole exercise exists to produce: every earlier number in this
    /// project timed a verifier run and called it an episode. A split that silently reported the
    /// total as turn time, or verify time as zero, would reproduce that error while looking
    /// plausible.
    #[cfg(target_os = "linux")]
    #[test]
    fn episode_splits_turn_time_from_verify_time() {
        let dir = std::env::temp_dir().join(format!("fc-episode-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("temp dir");

        // Turns take ~300ms, the verifier ~150ms, so the two are separable by a wide margin and the
        // assertions below cannot pass by coincidence.
        let script = dir.join("episode.sh");
        std::fs::write(
            &script,
            "#!/bin/busybox sh\n\
             /bin/busybox sleep 0.3\n\
             echo EPISODE_TURNS=14\n\
             echo EPISODE_TURNS_OK=13\n\
             echo EPISODE_VERIFY_START\n\
             /bin/busybox sleep 0.15\n\
             exit 0\n",
        )
        .expect("write script");

        let env_path = dir.join("fc-task.env");
        // FC_EPISODE_SHELL is the interpreter's absolute path plus any applet argument it needs.
        // busybox dispatches on argv[0] or its first argument, so "sh" is required here; a real
        // converted image records /usr/bin/bash instead.
        std::fs::write(
            &env_path,
            format!(
                "FC_EPISODE_SCRIPT={}\nFC_EPISODE_SHELL=/bin/busybox sh\n",
                script.to_string_lossy()
            ),
        )
        .expect("write env");

        let r = run_episode_from(&env_path.to_string_lossy()).expect("episode should run");

        assert_eq!(
            r.turns, 14,
            "turn count must come from the episode's own report"
        );
        assert_eq!(
            r.turns_ok, 13,
            "a rollout's own failed turn must be reported, not smoothed to the total"
        );
        assert_eq!(r.exit_code, 0, "the verifier's exit status is the reward");

        // Both components must be non-zero and roughly where the sleeps put them.
        assert!(r.verify_us > 100_000, "verify_us {} too small", r.verify_us);
        assert!(r.turns_us > 200_000, "turns_us {} too small", r.turns_us);
        // The split must not be the total attributed to one side. This is what fails if verify time
        // is folded into turn time, or vice versa.
        assert!(
            r.turns_us > r.verify_us,
            "turns ({}) took longer than verify ({}) in this fixture; the split is inverted",
            r.turns_us,
            r.verify_us
        );
        assert!(
            r.turns_us + r.verify_us <= r.total.as_micros() + 50_000,
            "turns {} + verify {} exceeds total {}",
            r.turns_us,
            r.verify_us,
            r.total.as_micros()
        );

        std::fs::remove_dir_all(&dir).ok();
    }

    /// An episode that never reaches its verifier is an error, not a fast episode.
    #[cfg(target_os = "linux")]
    #[test]
    fn episode_without_turn_markers_is_an_error() {
        let dir = std::env::temp_dir().join(format!("fc-episode-bad-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("temp dir");
        let script = dir.join("broken.sh");
        // Prints the *later* markers but not EPISODE_TURNS, which is what an episode killed between
        // them would do. This isolates the turn-count read specifically: a script printing nothing
        // at all would also fail on the second marker, so the weaker fixture cannot tell whether
        // the first read is checked at all.
        std::fs::write(
            &script,
            "#!/bin/busybox sh\necho EPISODE_TURNS_OK=13\necho EPISODE_VERIFY_START\nexit 3\n",
        )
        .expect("write");
        let env_path = dir.join("fc-task.env");
        std::fs::write(
            &env_path,
            format!(
                "FC_EPISODE_SCRIPT={}\nFC_EPISODE_SHELL=/bin/busybox sh\n",
                script.to_string_lossy()
            ),
        )
        .expect("write env");

        let err = run_episode_from(&env_path.to_string_lossy())
            .expect_err("an episode with no turn markers must not report success");
        assert!(
            err.contains("EPISODE_TURNS"),
            "error should name the missing marker, got: {err}"
        );

        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn run_task_takes_no_arguments() {
        // Extra tokens are ignored rather than rejected: the command comes from the image, so a
        // host that sent arguments is confused but not asking for something dangerous.
        assert!(matches!(
            parse_request("RUN_TASK extra"),
            Some(Request::RunTask)
        ));
    }

    #[test]
    fn parses_task_command_from_env_file() {
        let env =
            "FC_TASK_INSTANCE=django__django-11099\nFC_TASK_CMD=python -m pytest tests/foo.py\n";
        let cmd = parse_task_command(env).expect("should parse");
        assert_eq!(cmd, vec!["python", "-m", "pytest", "tests/foo.py"]);
    }

    #[test]
    fn task_command_tolerates_quotes_and_comments() {
        let env = "# written by docker-to-rootfs\nFC_TASK_CMD=\"/bin/sh -c true\"\n";
        assert_eq!(
            parse_task_command(env).expect("should parse"),
            vec!["/bin/sh", "-c", "true"]
        );
    }

    #[test]
    fn missing_task_command_is_none_not_panic() {
        assert!(parse_task_command("FC_TASK_INSTANCE=x\n").is_none());
        assert!(parse_task_command("").is_none());
    }

    /// The exit code must be the child's, not a constant.
    ///
    /// This is the reward signal. A SWE-bench instance is solved when its fail-to-pass tests exit
    /// zero, so a harness that always reported zero would mark every rollout as a success and the
    /// GRPO advantage would be computed over a constant. Both directions are asserted because only
    /// the non-zero case distinguishes a real read from a hardcoded 0.
    #[cfg(target_os = "linux")]
    #[test]
    fn task_exit_code_is_the_childs_not_a_constant() {
        let dir = std::env::temp_dir().join(format!("fc-task-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("temp dir");

        let write_env = |cmd: &str| {
            let p = dir.join("fc-task.env");
            std::fs::write(&p, format!("FC_TASK_CMD={cmd}\n")).expect("write env");
            p.to_string_lossy().into_owned()
        };

        let pass = run_task_from(&write_env("/bin/busybox true")).expect("pass should run");
        assert_eq!(pass.exit_code, 0, "a passing task must report 0");

        let fail = run_task_from(&write_env("/bin/busybox false")).expect("fail should run");
        assert_eq!(
            fail.exit_code, 1,
            "a failing task must report its own code, not 0"
        );

        // Peak RSS is non-zero and setup is a subset of total. The stronger claim -- that the peak
        // is the task's and not a prior task's -- lives in child_peak_rss_is_per_child_not_cumulative,
        // where it can be tested by difference. It cannot be tested by absolute value here: a child
        // inherits its parent's resident pages COW at fork, so ru_maxrss includes the test binary's
        // own footprint, which is tens of MiB where the real agent's is ~5.
        assert!(pass.peak_rss_kib > 0, "peak RSS was zero");
        assert!(pass.setup <= pass.total);

        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn parses_the_working_directory_field() {
        let env = "FC_TASK_CMD=/bin/true\nFC_TASK_WORKDIR=/testbed\n";
        assert_eq!(
            parse_task_field(env, "FC_TASK_WORKDIR").as_deref(),
            Some("/testbed")
        );
    }

    #[test]
    fn field_lookup_does_not_match_on_a_prefix() {
        // FC_TASK_CMD is a prefix of nothing here, but FC_TASK_WORKDIR must not be satisfied by
        // FC_TASK_WORKDIR_EXTRA or by FC_TASK_CMD's value. A substring match would silently hand
        // the child the wrong directory, and the task would fail in a way that looks like a
        // genuine zero reward.
        let env = "FC_TASK_WORKDIR_EXTRA=/wrong\nFC_TASK_WORKDIR=/right\n";
        assert_eq!(
            parse_task_field(env, "FC_TASK_WORKDIR").as_deref(),
            Some("/right")
        );
    }

    #[test]
    fn an_empty_field_reads_as_unset() {
        // An empty workdir must mean "do not chdir" rather than "chdir to the empty string",
        // which would fail with ENOENT and cost the task its reward.
        assert!(parse_task_field("FC_TASK_WORKDIR=\n", "FC_TASK_WORKDIR").is_none());
        assert!(parse_task_field("FC_TASK_WORKDIR=\"\"\n", "FC_TASK_WORKDIR").is_none());
        assert!(parse_task_field("", "FC_TASK_WORKDIR").is_none());
    }

    /// A missing env file must be an error the host can read, not a panic. As PID 1 a panic
    /// takes down the guest, and the host would see a microVM that stopped answering rather
    /// than an image that was built wrong.
    #[test]
    fn run_task_without_env_file_errors_clearly() {
        // TASK_ENV_PATH does not exist on a developer machine.
        let err = run_task().expect_err("no env file should be an error");
        assert!(
            err.contains("docker-to-rootfs"),
            "error {err:?} should point at how the image is built"
        );
    }

    /// **The exit code is the RLVR reward**, so its decoding is the most consequential logic in
    /// this file. A guard that reported 0 for a failed task would record every task as a pass
    /// and silently invert the reward signal.
    #[test]
    fn exit_code_decodes_normal_exits() {
        // waitpid encodes a normal exit as code << 8, low 7 bits clear.
        for code in [0, 1, 2, 42, 127, 255] {
            assert_eq!(
                exit_code_from_status(code << 8),
                code,
                "exit code {code} did not round-trip"
            );
        }
    }

    /// A test suite killed by the guest OOM killer dies on SIGKILL. Reporting that as 0 would
    /// turn an out-of-memory failure into a passing task, which is exactly the class of silent
    /// wrong answer this project keeps finding.
    #[test]
    fn exit_code_reports_signals_not_success() {
        for sig in [libc::SIGKILL, libc::SIGSEGV, libc::SIGTERM] {
            let got = exit_code_from_status(sig);
            assert_eq!(got, 128 + sig, "signal {sig} should map to 128+sig");
            assert_ne!(got, 0, "a signalled child must never report success");
        }
    }

    /// Zero must mean success and nothing else must, since the host compares against an expected
    /// exit code to compute pass rate.
    #[test]
    fn only_clean_exit_zero_is_success() {
        assert_eq!(exit_code_from_status(0), 0);
        assert_ne!(
            exit_code_from_status(1 << 8),
            0,
            "exit 1 must not read as 0"
        );
        assert_ne!(
            exit_code_from_status(libc::SIGKILL),
            0,
            "SIGKILL must not read as 0"
        );
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn spawn_and_wait_returns_the_child_exit_code() {
        // busybox true exits 0, false exits 1. If the code were discarded both would look alike.
        assert_eq!(
            spawn_and_wait("/bin/busybox", &["busybox", "true"], None).expect("true"),
            0
        );
        assert_eq!(
            spawn_and_wait("/bin/busybox", &["busybox", "false"], None).expect("false"),
            1
        );
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn spawn_and_wait_honours_the_working_directory() {
        // The child compares its own cwd, so its exit code reports where it actually ran. This is
        // the mutation that matters: a SWE-bench runner invoked from / cannot find its own tests,
        // and that failure would be recorded as a zero reward rather than a broken harness.
        let cwd_is = |dir: &str, cwd: Option<&str>| {
            spawn_and_wait(
                "/bin/busybox",
                &["busybox", "sh", "-c", &format!("test \"$(pwd)\" = {dir}")],
                cwd,
            )
            .expect("spawn")
        };
        assert_eq!(cwd_is("/etc", Some("/etc")), 0, "chdir did not take effect");
        // Negative control: without a cwd the child inherits the test runner's, which is the
        // package directory, so the same command must fail. Without this the assertion above
        // would also pass if every process on the machine happened to start in /etc.
        assert_eq!(
            cwd_is("/etc", None),
            1,
            "cwd was /etc even without being asked, so the assertion above proves nothing"
        );

        // A directory that cannot be entered must be distinguishable from a failed exec, so the
        // reward is not conflated with a misconfigured image.
        assert_eq!(
            spawn_and_wait(
                "/bin/busybox",
                &["busybox", "true"],
                Some("/nonexistent-dir-for-test")
            )
            .expect("spawn"),
            126,
            "an unusable workdir must report 126, not exec failure or success"
        );
    }

    /// wait4 reports each child's own peak, reflecting what that child allocated.
    ///
    /// This is the isolation the task RSS depends on. Tested by difference, not absolute value: a
    /// child inherits its parent's resident pages COW at fork, so every child's ru_maxrss carries
    /// the test binary's own footprint as a floor (tens of MiB here; the real agent's is ~5). Three
    /// broken implementations are all caught:
    ///   - VmHWM of the agent: identical for both children (each roughly the parent), so the hog
    ///     does not exceed the trivial child and the first assertion fails.
    ///   - RUSAGE_CHILDREN cumulative max: the trivial child run after the hog reports the hog's
    ///     peak, so the second assertion fails.
    ///   - correct wait4: the hog exceeds both trivial children by roughly what it allocated.
    #[cfg(target_os = "linux")]
    #[test]
    fn child_peak_rss_is_per_child_not_cumulative() {
        let dir = std::env::temp_dir().join(format!("fc-rss-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("temp dir");
        let script = dir.join("hog.sh");
        // ~500 MiB held resident: sort buffers its whole input, and 12M lines is well past any
        // plausible test-binary footprint, so the difference below cannot be COW inheritance.
        std::fs::write(
            &script,
            "#!/bin/busybox sh\nseq 1 12000000 | sort > /dev/null\n",
        )
        .expect("write script");
        let mut perms = std::fs::metadata(&script).expect("stat").permissions();
        std::os::unix::fs::PermissionsExt::set_mode(&mut perms, 0o755);
        std::fs::set_permissions(&script, perms).expect("chmod");

        let trivial_before =
            spawn_and_wait_full("/bin/busybox", &["busybox", "true"], None).expect("trivial runs");
        let hog = spawn_and_wait_full(
            "/bin/busybox",
            &["busybox", "sh", &script.to_string_lossy()],
            None,
        )
        .expect("hog runs");
        // A trivial child after the hog: a cumulative maximum would have it inherit the hog's peak.
        let trivial_after =
            spawn_and_wait_full("/bin/busybox", &["busybox", "true"], None).expect("trivial runs");

        // 256 MiB: below the hog's ~500 MiB allocation, above any test-binary COW floor.
        let margin = 256 * 1024;
        assert!(
            hog.peak_rss_kib > trivial_before.peak_rss_kib + margin,
            "hog {} KiB vs trivial {} KiB: the child's own allocation is not reflected",
            hog.peak_rss_kib,
            trivial_before.peak_rss_kib
        );
        assert!(
            trivial_after.peak_rss_kib + margin < hog.peak_rss_kib,
            "trivial-after-hog {} KiB is near the hog's {} KiB: peak RSS is cumulative, not per-child",
            trivial_after.peak_rss_kib,
            hog.peak_rss_kib
        );

        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn sockaddr_vm_layout_matches_kernel() {
        // sockaddr_vm is 16 bytes: family(2) + reserved1(2) + port(4) + cid(4) + zero(4).
        // A wrong size means bind() fails with EINVAL and the agent never becomes reachable.
        assert_eq!(std::mem::size_of::<SockAddrVm>(), 16);
    }
}
