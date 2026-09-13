# Changelog

All notable changes to this project will be documented in this file.

This project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> [!IMPORTANT]
> Major version zero (0.y.z) is for initial development. During initial development phase, expect breaking API and YAML schema changes during minor updates. Patch updates are guaranteed to be backward compatible during this phase.]

## Unreleased

## [0.3.0-rc.1] - 2026-09-13

### API

#### POST /processes/:processID/execution

- Added optional `tags` array to execution payload. Tags are stored with the job record and can be used to filter jobs when querying `/jobs`.

#### GET /jobs

- Job responses now includes the `tags` field.

#### GET /jobs/:jobID/metadata

- The `image` block now carries a `digestSource` field saying how the digest was determined.

#### GET /admin/resources

- Response now includes `usedGPUs`, `queuedGPUs`, `maxGPUs`, and a `gpus` array listing every device with its index, UUID, and the job holding it. The HTML view shows each device as free or in use rather than as a utilization bar, since which device is free is the question that matters. There are no GPU percentage fields, deliberately.

#### POST /processes/:processID/group-execution

- New endpoint to submit many jobs for one process in one request and get a single group ID to track all jobs. The whole request is validated before any job is created or response generated. Jobs are then created in the background.

#### GET /job-groups/:groupID

- New endpoint that reports a group's combined status, a count of its members by status, and one page of members in submission order. Members page with `limit` and `offset`, and filter with `status`, which accepts any job status plus `notCreated` for members whose job could not be created. Answers in HTML or JSON like other GET routes.

#### DELETE /job-groups/:groupID

- New endpoint that dismisses every member that is still accepted or running, in reverse submission order, leaving finished members untouched. Safe to repeat.

### Features

- Job groups feature is added, through the endpoints of this feature many jobs for one process can be submitted in one request and tracked as one unit, on every host type, with no change to how any member is queued or run. Every member remains an ordinary job at `/jobs/{jobID}` and carries a `group:{groupID}` tag, so a group's jobs are also findable through the ordinary `/jobs` tag filter. The `group:` tag prefix is now reserved for the server and rejected from client payloads. See [GROUPS_GUIDE.md](GROUPS_GUIDE.md).
- GPUs can now be requested by `docker` processes with `gpus` under `maxResources`, and are **enforced**: a job receives exactly the devices allocated to it, and no other job is placed on them. This is unlike `cpus` and `memory`, which remain advisory scheduling hints and are now documented as such. See [GPU_GUIDE.md](GPU_GUIDE.md).
- GPU requests that could never be satisfied are rejected at submission with `422` rather than queued indefinitely: more GPUs than the host has, or any GPUs on a `subprocess` process. Process registration is unaffected, so one catalog still loads on GPU and non-GPU hosts alike.
- Restart recovery reclaims the specific devices a surviving container holds, read back from the container itself, rather than a device count.

### Configuration

- New `MAX_GROUP_SIZE` environment variable (default: `1000`) capping how many jobs one job group may ask for. A larger request is refused.
- New `MAX_LOCAL_GPUS` environment variable (flag `-mlg`, default: all detected GPUs) capping how many GPUs the local job queue may allocate. Unlike `MAX_LOCAL_CPUS` and `MAX_LOCAL_MEMORY_MB`, a value that cannot be verified against detected hardware is fatal at startup, because GPUs are enforced and an over-claim would put two jobs on one card.
- New `SKIP_GPU_VERIFICATION` environment variable (flag `--skip-gpu-verify`, default: `false`) to trust `MAX_LOCAL_GPUS` without enumerating devices, for deployments where the API cannot see the GPUs it schedules onto. Note that a containerized SEPEX cannot see host GPUs unless the API container is itself given GPU visibility.
- New `SEPEX_DOCKER_NETWORK` environment variable (default: `sepex_net`) to set the docker network that launched job containers are attached to. Set it to `host` to run them with host networking, which is required on EC2 for instance profile credential access.
- AWS credentials (S3 and Batch) are now resolved through the default AWS credential chain rather than read directly from `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. Deployments that set those variables are unaffected, deployments without them can authenticate with an ECS/EC2 instance role, and `AWS_SESSION_TOKEN` is now honored for temporary credentials. MinIO continues to use its own `MINIO_*` keys.
- Optional new IAM permission `ecs:DescribeTasks` is added. It lets AWS Batch jobs record the digest ECS actually pulled. If not provided, jobs using a moving tag fall back to asking the registry and record `tag-lookup`.
- Processes are now warned about at startup when their references are not pinned, meaning an `aws-batch` job definition without a revision, or an image that is a moving tag rather than `repository@sha256:...`. These are advisory only and do not stop a process from loading.

### Fixed

- The active jobs map was read without its lock by every job endpoint, while other goroutines added to and removed from it. A status request arriving as a job started or finished could take the server down. Reads now go through a locked accessor.
- AWS Batch log stream lookup read the region from `AWS_DEFAULT_REGION`, which the API never defines, leaving the request without a region. All AWS calls now resolve the region from `AWS_REGION`.
- The erroneous Docker Hub digest lookup was removed and will be implemented later.

## [0.2.2] - 2025-2-28

### API

#### POST /processes/:processID/execution

- Execution mode now determined per OGC API - Processes Requirements 25/26: honors `Prefer: respond-async` header when process supports both modes, defaults to sync otherwise
- Returns `Preference-Applied` response header when async preference is honored

#### GET /admin/resources

- New endpoint to view resource utilization for local jobs (docker, subprocess) and queue status

#### GET /jobs/:jobID/metadata

- Added `recoveryNotice` object to metadata format

### Features

- Added restart recovery for docker, subprocess, and AWS Batch jobs, including status reconciliation and log handling.
- Introduced `lost` job status and surfaced it in UI status indicators (job list, job logs, status table).
- Recovered jobs annotate metadata with a recovery notice and write best-effort metadata when some fields are missing.
- Resource pool can force-reserve resources for already-running recovered docker jobs.

### Configuration

- New `MAX_LOCAL_CPUS` and `MAX_LOCAL_MEMORY` environment variables (or `--max-local-cpus` and `--max-local-memory` CLI flags) to set resource limits for local job scheduling
- Process definitions are validated against these limits at startup and when adding/updating processes via API
- Processes without explicit resource requirements use default values

### Documentation

- Added sequence diagram for local scheduler
- Added Recovery section to DEV_GUIDE

## [0.2.1] - 2025-12-03

### API

- Version information is added in landing page.

### Configuration

- Repository URL is now configurable via `REPO_URL` environment variable. This URL is used for version links and metadata context references.

### Documentation

- Changelog updated to new format

## [0.2.0] - 2025-12-02

### API

#### GET /jobs/:jobID/logs

- In response body `container_logs` key is replaced by `process_logs`

#### PUT|POST /processes/:processID

- Request payload schema has changed (See Process YAML Schema changes below)

### Process YAML Schema

- `command` is now a first class object and moved outside of `container`
- `config` object is added
- `maxResources` and `envVars` are moved under `config` object
- `image` moved under `host`
- `container` object removed
- `host.type` valid options are changed from `local` | `aws-batch` to `docker` | `aws-batch` | `subprocess`

### Features

- `subprocess` type processes now can be executed through API. They must be registered like other processes and will be called using OS subprocess calls.

### Documentation

- A `CHANGELOG.md` file is added in the repo.
- Process templates are provided for all three host types in `./process_templates` folder
- Windows setup instructions are added in `README.md`

## [0.1.0] - 2023-07-07

- Initial release with core API endpoints for process and job management

[Unreleased]: https://github.com/Dewberry/sepex/compare/v0.3.0-rc.1...HEAD
[0.3.0-rc.1]: https://github.com/Dewberry/sepex/releases/tag/v0.3.0-rc.1
[0.2.2]: https://github.com/Dewberry/sepex/releases/tag/v0.2.2
[0.2.1]: https://github.com/Dewberry/sepex/releases/tag/v0.2.1
[0.2.0]: https://github.com/Dewberry/sepex/releases/tag/v0.2.0
[0.1.0]: https://github.com/Dewberry/sepex/releases/tag/v0.1.0