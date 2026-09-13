# SEPEX - Service for Encapsulated Processes Execution

[![Container CI](https://github.com/Dewberry/sepex/actions/workflows/release.yml/badge.svg)](https://github.com/Dewberry/sepex/actions/workflows/release.yml)
[![E2E Tests](https://github.com/Dewberry/sepex/actions/workflows/e2e-tests.yml/badge.svg?event=push)](https://github.com/Dewberry/sepex/actions/workflows/e2e-tests.yml)
[![Update Sequence Diagrams Wiki](https://github.com/Dewberry/sepex/actions/workflows/update-squence-wiki.yml/badge.svg)](https://github.com/Dewberry/sepex/actions/workflows/update-squence-wiki.yml)
[![Dev Image](https://github.com/Dewberry/sepex/actions/workflows/build-dev-image.yml/badge.svg)](https://github.com/Dewberry/sepex/actions/workflows/build-dev-image.yml)

An [OGC API - Processes](https://docs.ogc.org/is/18-062r2/18-062r2.html#toc0) compliant server for executing encapsulated (containerized, CLI, ...) processes locally or on a cloud at scale.

SEPEX provides:

- job queuing
- job tracking
- job logs
- job metadata
- job results

The API responses follow the examples provided here:
https://developer.ogc.org/api/processes/index.html

---

## Getting Started

### Linux (Building Docker Image from Source)

Common tasks are wrapped in a [justfile](justfile). Install [`just`](https://github.com/casey/just) and run `just` to list them; if you would rather not install it, the underlying commands are all visible in that file.

1. Create a `.env` file (example below) at the root of this repo.

![](imgs/readme/getting-started.gif)

1. Add/Delete process configuration file(s) (yaml) to the [plugins](plugins/) directory as needed
1. Build docker images for example plugins with `just build-plugins`
1. Start the stack with `just up`. This creates the `sepex_net` docker network, brings up MinIO, Postgres and the API, and creates the bucket named by `STORAGE_BUCKET` in MinIO.
1. Test endpoints using the swagger documentation page. (http://localhost:5050/swagger/index.html)

Other tasks:

| Command         | What it does                                                                  |
| --------------- | ----------------------------------------------------------------------------- |
| `just logs`     | Follow the stack logs                                                         |
| `just down`     | Stop the stack                                                                |
| `just wipe`     | Stop the stack and delete all local data under `.data/`                       |
| `just test-e2e` | Run the end-to-end suite against the local compose stack, then print the logs |

`just test-e2e` runs against the `.env` already at the repo root, so make sure it points at the local stack before running it. It leaves the stack running afterwards; use `just down` when you are finished with it.

![](imgs/readme/swagger-demo.gif)

### Windows

*`docker` and `aws-batch` jobs have not been tested yet on Windows*

1. Create a `.env` file (example below). Update paths in the env file as needed.
2. Download and run MinIO https://min.io/docs/minio/windows/index.html
3. Create a bucket in MinIO Web console
4. In a separate command prompt window, CD into api folder. Run `cd api`
5. Build API by running `go build -o sepex.exe main.go`
6. Run API by `sepex -e ../.env`

### Prebuilt Images

The API image is published to GitHub Container Registry, so you can run a build
without compiling it yourself:

```sh
docker pull ghcr.io/dewberry/sepex:dev
```

| Tag | Points at |
| --- | --- |
| `dev` | The latest commit on `main`. Moves on every push that touches `api/`. |
| `sha-<short-sha>` | One specific commit. Does not move. |
| `latest` | The most recent non-prerelease release. |
| `0.2.2`, `0.2` | A released version, exact or latest patch of that minor. |

Pin to a released version for a reproducible setup as `dev` and `latest` are movable and `sha-` tags older than two weeks are pruned.

---

## System Components

The system design consists of four major system components:

### API

The API is the main orchestrator for all the downstream functionality and a single point of communication with the system.

### Processes

![](imgs/readme/processes.png)
Processes are computational tasks described through a configuration file that can be executed as a subprocess or in a container. Each configuration file contains information about the process such as the title of this process, its description, execution mode, execution resources, secrets required, inputs, and outputs. Each config file is to be unmarshalled to register a process in the API. These processes then can be called several times by the users to run jobs.

### Jobs

![](imgs/readme/jobs.png)
Each execution of a process is called a job. A job can be synchronous or asynchronous depending on the process configuration and client preference. Synchronous jobs return responses after the job has reached a finished state, meaning either successful or failed. The asynchronous jobs return a response immediately with a job id for the client so that the client can monitor the jobs.

*Note on Processes: The developers must make sure they choose the right platform to execute a process. The processes that are short-lived and fast and do not create a file resource as an output, for example getting the water surface elevation values for a coordinate from cloud raster, must be registered to run on the local machine so that they are synchronous. These kinds of processes should output data in JSON format.*

*On the other hand, processes that take a long time to execute and their results are files, for example clipping a raster, must be registered to run on the cloud so that they are asynchronous. These processes should contain links to file resources in their results.*

### Execution Platforms

Execution platforms are hosts that can provide resources to run a job. This can be a cloud provider such as AWS Batch or the local machine.

## Behaviour

![](imgs/readme/design.svg)

At the start of the app, all the `.yaml` `.yml` (configuration) files are read and processes are registered. Each file describes what resources the process requires and where it wants to be executed. There are three execution platforms available; docker processes run in a docker container, hence they must specify a docker image and the tag. The API will download these images from the repository and then run them on the host machine. Commands specified will be appended to the entrypoint of the container. The API responds to the request of local processes synchronously.

Cloud processes are executed on the cloud using a workload management service. AWS Batch was chosen as the provider for its wide user base. Cloud processes must specify the provider type, job definition, job queue, and job name. The API will submit a request to run the job to the AWS Batch API directly.

Subprocess-based processes are executed natively using an OS subprocess call.

All processes must expect a JSON load as the last argument of the command and write results as the last log message in the format `{"plugin_results": results}`. It is the responsibility of the process to write these results correctly if the process succeeds. The API will store logs of the container and will try to parse the last log for results when the client requests results for jobs.

When a local job (docker or subprocess) reaches a finished state (successful or failed), the artifacts of the jobs such as the container are removed. Similarly, if an active job is explicitly dismissed using DEL route, the job is terminated, and resources are freed up. If the server is gracefully shut down, all currently active jobs are terminated, and resources are freed up.

The API responds to all GET requests as HTML or JSON depending upon if the request is being originated from Browser or not or if it specifies the format using query parameter ‘f’.

### Resource Management

Local jobs (docker and subprocess) declare what they need in their process
config, and the queue uses those numbers to decide how many jobs run at once.
How strictly each resource is held to differs, and the difference is deliberate:

| Resource | Docker       | Subprocess    | AWS Batch |
|----------|--------------|---------------|-----------|
| `cpus`   | advisory     | advisory      | ignored   |
| `memory` | advisory     | advisory      | ignored   |
| `gpus`   | **enforced** | not supported | ignored   |

`cpus` and `memory` are an honor system. The declared values inform scheduling
only; nothing prevents a process from exceeding them at runtime. Docker's CPU
limit throttles rather than reserves and its memory limit kills rather than
reserves, so neither can guarantee a job the capacity it asked for. Using them
purely for scheduling is the honest option.

`gpus` is enforced, because a GPU cannot work that way: GPU memory does not
overflow gracefully, a device cannot be subdivided, and a container is given no
GPU at all unless one is requested explicitly. SEPEX therefore allocates whole
devices and tells Docker exactly which ones a container may use.

AWS Batch jobs use no local resources; their allocation comes from the Batch
job definition.

See [GPU_GUIDE.md](GPU_GUIDE.md) for declaring GPUs, configuring the server,
and troubleshooting.

### Job Groups

Work that is naturally many independent jobs can be submitted in one request and
tracked as one unit:

```
POST /processes/{processID}/group-execution
```

The response carries one group ID. `GET /job-groups/{groupID}` reports a
combined status and a count of members by status, and
`DELETE /job-groups/{groupID}` dismisses every member that has not finished.

A group is only a submission and tracking mechanism. Every member is an ordinary
job with its own status, logs, results and metadata at `/jobs/{jobID}`, and it
runs exactly as it would have if submitted on its own. A group has no logs,
results or metadata of its own. The groups are not meant to orchestrate workflows or make jobs depend on each other, and changes nothing about how any member is queued or executed.

See [GROUPS_GUIDE.md](GROUPS_GUIDE.md) for submitting, tracking, dismissing, and
what happens when a submission cannot be completed.

### Logs

![](imgs/readme/logs.png)
Logs are not included in the OGC-API Processes specification, however, for this implementation we have added logs to provide information on the API and Containers.

### Metadata

![](imgs/readme/metadata.png)
Similar to logs, metadata is not included in the OGC-API Processes specification. We have added metadata as an endpoint to provide information on the version of the plugin, the runtime, and the input arguments passed to the container at runtime. Metadata is generated for only successful jobs.

The `image` block in `metadata` object record which container image the job ran. Because a tag such as `:latest` in processes yaml or AWS Batch job definition can be repointed at any time, the recorded `imageDigest` is not always proof of what executed. A `digestSource` says how it was determined:

| `digestSource` | Meaning | Reliability |
| --- | --- | --- |
| `pinned` | The digest was already part of the image reference in process yaml or job definition | Exact |
| `observed` | The executor reported the digest it actually pulled: the Docker daemon locally, or ECS for AWS Batch. | Exact |
| `tag-lookup` | We asked the registry what the tag pointed at while the job was running. Almost certainly what ran, but inferred rather than observed. | Inferred |
| `unavailable` | No digest could be determined. No digest is recorded rather than an unjustified one. | None |


## Example .env file

An env file is required and should be available at the root of this repository (`./.env`). See the [example.env](example.env) for a guide.

## Notes

1. API docs created using [swaggo](https://github.com/swaggo/swag)
