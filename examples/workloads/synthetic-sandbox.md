# Synthetic sandbox episode

A parameterised, self-contained workload that needs no external image, dataset, or network. It
is the workload behind the guest agent's `RUN_SANDBOX` verb, and the default the `dispatch`
example sends.

## What it models

An agentic coding sandbox turn is dominated by three costs, and this workload reproduces all
three without shipping a real toolchain:

| Cost | How the workload creates it | Parameter |
|---|---|---|
| Fork/exec churn | Spawns short-lived child processes, one per step | `steps` |
| Resident memory | Touches a working-set buffer so pages are actually resident, not just reserved | `resident_kib` |
| File writes | Writes a buffer to the guest filesystem each step, exercising writeback | `write_kib` |

These are the parts of a sandbox turn that scale with microVM density: they compete for the
shared page cache, the writeback path, and the process table. A register-bound compute loop
would not, which is why the sweep harness distinguishes `RUN` (compute) from `RUN_SANDBOX`
(sandbox-shaped) work.

## Wire protocol

```
request:  RUN_SANDBOX <resident_kib> <steps> <write_kib>
reply:    OKS <resident_kib> <steps> <write_kib> <total_us> <spawn_us> <write_us> <spawns_ok> <checksum> <peak_rss_kib>
```

The guest echoes the three request parameters back so the host can confirm the reply matches
what it asked for. `spawns_ok` is the count of child processes that exited cleanly; a value below
`steps` signals the guest ran out of resources under density pressure. `checksum` proves the
memory buffer was actually written and read rather than optimised away. `peak_rss_kib` is the
agent's own high-water resident set (`VmHWM` from `/proc/self/status`): the sandbox holds its
resident buffer in the agent process and spawns only trivial children, so this is the one verb
where the agent's own footprint, not a child's, is the figure that matters. A value of 0 means
"not measured", not "no memory used".

## Definition used by the examples

```json
{
  "name": "synthetic-sandbox",
  "verb": "RUN_SANDBOX",
  "resident_kib": 65536,
  "steps": 20,
  "write_kib": 256,
  "notes": "128 MiB guest default leaves headroom above a 64 MiB resident set."
}
```

`resident_kib` must stay below the guest's memory size (the guest agent refuses working sets
larger than 64 MiB to avoid an OOM kill that would present to the host as a microVM that simply
stopped answering). The default 65536 KiB (64 MiB) is the ceiling; lower it if you run guests
smaller than the 128 MiB default.

## Run it

Against a live microVM's vsock socket (see `../README.md` for how to get one):

```sh
# From examples/
go run ./dispatch \
  -vsock /srv/jailer/firecracker/vm0/root/v.sock \
  -workload sandbox -resident-kib 65536 -steps 20 -write-kib 256
```

To drive the same workload across the full placement/density matrix and get latency
distributions rather than a single reply, use the lab sweep harness instead:

```sh
# From lab/  (needs root and a prepared host)
sudo go run ./cmd/labsweep \
  -configs B,D -densities 10,45,90 \
  -resident-kib 65536 -steps 20 -write-kib 256 \
  -out results/
```

The sweep is the "Lab Sweep" component from the architecture diagram: it identifies the best
placement configuration and pinning for this workload, which an operator then feeds to the host
agent's `-config` flag.
