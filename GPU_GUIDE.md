# GPU Support

How to give a process a GPU, what SEPEX guarantees when it does, and what it
deliberately does not.

- [In short](#in-short)
- [Why GPUs are treated differently](#why-gpus-are-treated-differently)
- [Declaring GPUs in a process](#declaring-gpus-in-a-process)
- [What your process sees](#what-your-process-sees)
- [Image requirements](#image-requirements)
- [Configuring the server](#configuring-the-server)
- [Running SEPEX itself in a container](#running-sepex-itself-in-a-container)
- [What happens when a job is submitted](#what-happens-when-a-job-is-submitted)
- [Monitoring](#monitoring)
- [Restarts and recovery](#restarts-and-recovery)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## In short

Add `gpus` to a docker process's resource block:

```yaml
config:
  maxResources:
    cpus: 2
    memory: 4096
    gpus: 1
```

Set `MAX_LOCAL_GPUS` on the server (or let it auto-detect), and jobs for that
process each receive one whole GPU, exclusively, for their entire run.

Unlike `cpus` and `memory`, this is **enforced**. No other job will be placed
on that device.

## Why GPUs are treated differently

`cpus` and `memory` are an **honor system**. The numbers only tell the job
queue how many jobs may run at once; nothing stops a process from exceeding
them. That is deliberate. Docker's CPU limit throttles rather than reserves,
its memory limit kills rather than reserves, and neither can promise a job the
capacity it asked for. Since the declared values cannot buy a guarantee, using
them purely for scheduling is the honest option.

`gpus` is **enforced**, because a GPU cannot be treated that way:

- **GPU memory does not overflow gracefully.** There is no swap. Two jobs on
  one card do not slow down, they fail with out-of-memory — and which one fails
  is a race, so the same pair of jobs can succeed one day and fail the next.
- **A GPU cannot be subdivided.** There is no way to cap how much of a card a
  container uses. A `gpuMemory` setting would be a promise nothing could keep.
- **Containers get no GPU by default.** A GPU-aware process in an ordinary
  container finds zero devices and quietly falls back to CPU. It does not
  error. It is simply slow, with nothing in the logs to say why.

So GPUs are handed out as whole devices, and SEPEX tells Docker exactly which
ones a container may touch.

| Resource | Docker    | Subprocess    | AWS Batch |
|----------|-----------|---------------|-----------|
| `cpus`   | advisory  | advisory      | ignored   |
| `memory` | advisory  | advisory      | ignored   |
| `gpus`   | **enforced** | not supported | ignored   |

For AWS Batch, GPUs come from the Batch job definition, exactly as CPU and
memory already do. Declaring `gpus` there has no effect.

## Declaring GPUs in a process

`gpus` is a whole number and defaults to `0`. There are no fractional GPUs.

```yaml
info:
  id: train-model
  version: 1.0.0

host:
  type: docker
  image: myorg/trainer:1.4.0

config:
  maxResources:
    cpus: 4
    memory: 16384
    gpus: 1
```

A process needing more than one device asks for more:

```yaml
    gpus: 2
```

Bear in mind a multi-GPU job only runs when that many devices are free at once,
and see [Limitations](#limitations) for how that interacts with the queue.

**Declaring GPUs does not stop a process registering on a host that has none.**
That is intentional: the same process catalog is meant to load on GPU and
non-GPU machines alike. The server logs a warning at registration, and refuses
the job at submission instead — see
[What happens when a job is submitted](#what-happens-when-a-job-is-submitted).

## What your process sees

**Your process does not choose a device, and does not need to know which one it
was given.**

Ask for one GPU and your process sees exactly one, numbered `0`. Ask for two
and it sees `0` and `1`. This holds no matter which physical cards SEPEX
allocated — the container's view is renumbered from zero.

So ordinary code works unchanged:

```python
import torch

print(torch.cuda.device_count())     # 1 for `gpus: 1`
device = "cuda" if torch.cuda.is_available() else "cpu"
```

Do not set `CUDA_VISIBLE_DEVICES` yourself, and do not hardcode a device index
other than what you can see. SEPEX has already narrowed the container's view;
overriding it can only take you outside your allocation, where another job may
be running.

The job log records which physical devices were used, so a run can be traced
back afterwards:

```
Allocated GPUs: indices [1] device ids [GPU-4a5b6c7d-1234-5678-9abc-def012345678]
```

## Image requirements

**SEPEX provides the device and the driver. It does not provide CUDA.**

The NVIDIA Container Toolkit injects the driver and device nodes into your
container, but the CUDA runtime libraries have to be in the image already.
Build on a base that includes them:

```dockerfile
FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04
```

or use a framework image that bundles CUDA, such as an official PyTorch or
TensorFlow GPU image. A plain `python:3.12` image will be given a GPU and be
unable to use it.

## Configuring the server

| Setting | Flag | Default |
|---|---|---|
| `MAX_LOCAL_GPUS` | `-mlg` | every GPU detected |
| `SKIP_GPU_VERIFICATION` | `--skip-gpu-verify` | `false` |

At startup SEPEX runs `nvidia-smi` to enumerate the GPUs on the host, and logs
what it found:

```
ResourceLimits initialized: maxCPUs=6.40, maxMemory=8192MB, maxGPUs=2
```

Leaving `MAX_LOCAL_GPUS` unset uses every detected device. Setting it to a
smaller number reserves the remainder for other things on the machine; the
lowest-numbered devices are the ones SEPEX uses. Setting it to `0` disables GPU
scheduling entirely.

**A value that cannot be verified is a startup error.** Asking for more GPUs
than were detected, or asking for any when detection did not work at all, stops
the server rather than accepting jobs it could not really run:

```
MAX_LOCAL_GPUS is 2 but no GPU could be detected on this host. If this host has
GPUs, ensure the NVIDIA drivers are installed; if SEPEX itself is running in a
container, that container must also be given GPU visibility (`--gpus all`, or
compose `deploy.resources.reservations.devices`). Where the API genuinely
cannot see the GPUs it schedules onto, set SKIP_GPU_VERIFICATION=true
(--skip-gpu-verify) together with MAX_LOCAL_GPUS to trust that count
unverified. Set MAX_LOCAL_GPUS=0 to run without GPUs.
```

This is stricter than `MAX_LOCAL_CPUS` and `MAX_LOCAL_MEMORY_MB`, which are
never checked against real hardware and fall back to a default when
unparseable. The difference is deliberate: those two are advisory, so an
over-claim only affects scheduling. An over-claimed GPU count would put two
jobs on one card.

### SKIP_GPU_VERIFICATION

Set this when SEPEX genuinely cannot see the GPUs it schedules onto. It skips
detection, makes `MAX_LOCAL_GPUS` authoritative, and requires it to be set:

```bash
MAX_LOCAL_GPUS=2
SKIP_GPU_VERIFICATION=true
```

The tradeoff is that devices are then addressed by index rather than by UUID.
Indices are stable in normal operation but do not survive hardware changes the
way UUIDs do. Prefer giving SEPEX visibility of the GPUs where you can, and use
this when you cannot.

## Running SEPEX itself in a container

This is the most common reason detection reports zero GPUs on a machine that
has them.

The provided compose stack runs the API in a container with only the Docker
socket mounted. It launches job containers as *siblings* on the host daemon —
so those containers can be given GPUs, while the API itself cannot see any.

Give the API container GPU visibility so it can enumerate. Visibility is not
allocation: the API never runs GPU work, so this costs nothing at runtime.

```yaml
services:
  api:
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
```

If that is not possible in your environment, use `SKIP_GPU_VERIFICATION` with
an explicit `MAX_LOCAL_GPUS` instead.

## What happens when a job is submitted

**The process needs more GPUs than the host has** — rejected immediately with
`422`, because no amount of waiting would help:

```json
{"message": "process train-model requires 2 GPU(s) but this host has 0; the job can never be scheduled here"}
```

**The process is a subprocess process declaring GPUs** — also `422`. GPU
allocation is not supported there; see [Limitations](#limitations).

**Enough GPUs exist but are busy** — the job behaves like any other queued job.
Async jobs wait in the queue and start when devices free up. Sync jobs cannot
wait, so they return `503`:

```json
{"message": "All GPUs are currently allocated. A GPU is held for a job's entire run, so this may not clear soon; use async-execute mode (if available for this process) to wait in the queue instead."}
```

That wording is not boilerplate. A GPU is held for the whole of a job's run, so
a card occupied by a long training job may not free for hours — retrying a sync
request is usually the wrong move.

**Devices are available** — the job is allocated the lowest-numbered free
devices, and they are exclusively its own until it finishes, fails, or is
dismissed.

## Monitoring

`/admin/resources` shows every device individually rather than as a percentage,
because the useful question is *which* device is free, not how many:

```
GPUs (1 / 2 in use)

  ┌──────────────┐  ┌──────────────┐
  │ GPU 0        │  │ GPU 1        │
  │ in use by    │  │ free         │
  │ 3f2a1b8c     │  │              │
  │ GPU-4a5b6c7d │  │ GPU-9e8d7c6b │
  └──────────────┘  └──────────────┘

Queued GPUs: 3
```

The job ID links to the job holding the device. The same data is available as
JSON:

```json
{
  "usedGPUs": 1,
  "queuedGPUs": 3,
  "maxGPUs": 2,
  "gpus": [
    {"index": 0, "uuid": "GPU-4a5b...", "jobID": "3f2a1b8c-..."},
    {"index": 1, "uuid": "GPU-9e8d..."}
  ]
}
```

`queuedGPUs` is the total requested by jobs still waiting, so it can exceed
`maxGPUs`.

## Restarts and recovery

If SEPEX restarts while a GPU job is running, the container keeps running and
SEPEX re-adopts it. To know which devices that container holds, SEPEX asks
Docker: a container records the devices it was created with, and that record
survives a restart. The job is then waited on normally and finishes with its
real exit code.

```
Recovery(docker): job=3f2a1b8c reclaimed GPU device(s) [1]
```

Occasionally SEPEX cannot match a container's devices against its own — most
often because `MAX_LOCAL_GPUS` or `SKIP_GPU_VERIFICATION` changed while that
job was running, or because the hardware changed. It says so and does not
reserve them:

```
Recovery(docker): job=3f2a1b8c is running on GPU device(s) [GPU-9f8e...] that
this pool does not recognise, so they will NOT be reserved. If they are in fact
devices this pool can allocate, a later job may be placed on the same hardware
and fail with an out-of-memory error. Check whether MAX_LOCAL_GPUS or
SKIP_GPU_VERIFICATION changed since this job started.
```

**This warning is worth acting on.** If those really were devices SEPEX manages,
a later job may be scheduled onto hardware that is already busy and die with a
CUDA out-of-memory error. Avoid changing GPU settings while GPU jobs are
running, and if you must, let them drain first.

## Limitations

**No fractional or memory-based allocation.** You cannot request half a GPU or
a number of gigabytes. Docker offers no way to enforce either, so SEPEX does
not pretend to. MIG and MPS are not supported.

**Subprocess processes cannot use GPUs.** Their jobs are rejected at
submission. Nothing prevents a subprocess from *reaching* a GPU, which is
exactly the problem: SEPEX could not stop it colliding with a docker job that
holds one. Refusing is more honest than allocating something it cannot enforce.

**NVIDIA only.** AMD ROCm and Intel GPUs are not supported.

**The queue is strictly first-in, first-out.** A queued job that cannot start
blocks the jobs behind it, even ones needing no GPU at all. On a two-GPU host,
a waiting `gpus: 2` job holds up everything until both devices free. Keep this
in mind when mixing long GPU jobs with short CPU ones on the same instance.

**GPU memory is not tracked.** A single job can exhaust its own card. Whole
devices are allocated precisely because how much memory a job needs is not
knowable in advance.

## Troubleshooting

**`GPU detection found 0 GPUs` on a machine that has them.**
Either the NVIDIA drivers are not installed, or SEPEX is running in a container
without GPU visibility — see
[Running SEPEX itself in a container](#running-sepex-itself-in-a-container).
Check `nvidia-smi` from wherever the API process runs, not just from the host
shell.

**The server refuses to start, citing `MAX_LOCAL_GPUS`.**
The value is higher than the number of devices detected, or detection failed
entirely. Fix visibility, lower the value, set `MAX_LOCAL_GPUS=0`, or set
`SKIP_GPU_VERIFICATION=true` if the API genuinely cannot see them.

**A job is rejected with 422.**
The host has fewer GPUs than the process requires, or it is a subprocess
process. Check `maxGPUs` at `/admin/resources`.

**The job runs, but the process reports no GPU.**
Almost always the image. The CUDA runtime libraries must be in it — see
[Image requirements](#image-requirements). Confirm by running `nvidia-smi`
inside the container: the toolkit injects it, so if `nvidia-smi` works but CUDA
does not, the device arrived and the libraries are missing.

**A job fails with CUDA out-of-memory.**
Either it genuinely needs a larger card, or something outside SEPEX is using
the same device. Check `nvidia-smi` on the host for processes SEPEX did not
start. If it began after a restart, look for the recovery warning in
[Restarts and recovery](#restarts-and-recovery).

**Jobs are queued while GPUs sit idle.**
Most likely head-of-line blocking: a job at the front of the queue needs more
GPUs than are free, and holds up everything behind it. `/admin/resources` shows
both free devices and `queuedGPUs`.
