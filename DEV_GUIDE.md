## Config

- All secrets and configuration settings are handled through environment variables
- There is an example.env provided to ease the configuration process
- Command line flags are available for config that is only needed at startup, they take precedence over the environment variables when used.
- Other configs are defined through env variables so that they can be modified without restarting the server.
- Here is the resolution order:
    - Flag, where option is available and used
    - Environment variable
    - Default value, where available

## Process Specific Env

- They must start with ALL CAPS process id.
- They will be passed to jobs with process id prefix removed. This allow setting 3rd party env variables such as GDAL_NUM_CPUS etc. without collision risk.
- We are parsing at the job level so as to allow dynamic updates without having to restart server.

## Auth

- If auth is enabled some or all routes are protected based on env variable `AUTH_LEVEL` settings.
- The middleware validate and parse JWT to verify `X-SEPEX-User-Email` header and inject `X-SEPEX-User-Roles` header.
- A user can use tools like Postman to set these headers themselves, but if auth is enabled, they will be checked against the token. This setup allows adding submitter info to the database when auth is not enabled.
- If auth is enabled `X-SEPEX-User-Email` header is mandatory.
- Requests from Service Role will not be verified for `X-SEPEX-User-Email`.
- Only service_accounts can post callbacks
- Requests from Admin Role are allowed to execute all processes, non-admins must have the role with same name as `processID` to execute that process.
- Requests from Admin Role are allowed to retrieve all jobs information, non admins can only retrieve information for jobs that they submitted.
- Only admins can add/update/delete processes.

## Inputs

- If `"inputs": {}` in `/execution` payload, nothing will be appended to process commands. This allows running processes that do not have any inputs.
- `"tags"` (optional) can be included as an array of strings in the `/execution` payload. Tags are stored with the job record and can be used to filter jobs when querying `/jobs`. If omitted, the job will be created with an empty tag list.

Example payload:

```JSON
{
  "inputs": {
    "text": "Hello World"
  },
  "tags": ["example", "test"]
}
```

## Scope

- The behavior of logging is unknown for AWS Batch processes with job definitions having number of attempts more than 1.

## Local Scheduler

**Design decisions:**

1. ResourceLimits calculated once at startup from flags/env vars. Dynamic reconfiguration rejected because a queued job could block forever if limits are reduced below its requirements after it was already validated and enqueued.
1. ResourcePool and PendingJobs use `sync.Mutex`. Channels add complexity without benefit for simple state. Go channels use internal mutexes anyway, so performance is similar.

## Recovery

Recovery rebuilds in-memory state after a restart using DB non-terminal jobs.

**Design decisions:**

1. All non terminal jobs before restart get a log entry in logs and if recovered successfully get a note in metadata as well.
1. Docker jobs that never started don't have enough data to be requeued so they are marked dismissed. Jobs whose container can't be found/accessed are marked LOST, while jobs whose container can be found and accessed are recovered.
1. Subprocess jobs are not recoverable. They are marked DISMISSED if prior status is ACCEPTED, this is because we know for sure the weren't RUNNING before crash. Jobs that were RUNNING are considered LOST.
1. AWS Batch recovery queries the Batch API for current state. Missing jobs are marked LOST; running jobs are re-registered in ActiveJobs; finished jobs are finalized and metadata/log handling is triggered.
1. Metadata for recovered jobs will be incomplete but present.

## Image Provenance

How we ended up recording what each job ran, and what we tried first.

**Design decisions:**

1. We chose the Docker model for image provenance: loose references, no enforcement, but an honest record of what ran. This is to allow fast integration of newest container images, while being honest about it that means we can not always reliably record provenance for cloud jobs.This choice forced us to add `digestSource` in metadata, to list our trust in the value that we are providing.
2. With this change , our capturing of digest moved from job completion to the RUNNING transition. Because after job is completed, is not the right time to do tag based lookup, drift window is wider at that point.
3. We are aiming to provide "image index" digest, but if a developer list manifest digest, then that will be adopted. We are calling it ImageDigest in metadata as a general term (technically incorrect, but easier for understanding).

The `digestSource` values are documented for users in the Metadata section of README.md and declared as a term in context.jsonld. We have to keep those and the constants in api/jobs/metadata.go in sync.

## Job Groups

They are a submission and tracking mechanism only: every member is an
ordinary job.

**Design decisions:**

1. A group is a vendor extension at its own path, the shape of its endpoints (`/processes/{processID}/group-execution`, and then served at `/job-groups/{groupID}`) is kept same as an OGC job, which is created at an execution endpoint and then lives under `/jobs`.
1. The jobs table is not altered. Groups live in two new tables. An existing database needs no migration. That matters most for SQLite, which has no `ADD COLUMN IF NOT EXISTS` and therefore no backfill path in that backend.
1. Members are keyed by position rather than by job, and `job_id` is nullable. A member whose job could not be created keeps its place and carries the reason, so a client can see which entries of its request to resubmit instead of inferring them from gaps.
1. Submission runs in the background and the request returns as soon as the group is recorded. This is because submissions will take time and we can not let callers wait that much.
1. Once submission starts, a member that fails does not stop the ones after it, because killing jobs that are running correctly throws away real work. Submission gives up only after 10 consecutive failures assuming that backend is broken.
1. SEPEX never dismisses a group's members on its own; only DELETE does. A partially submitted group leaves its members running and lets the client decide whether the partial result is worth keeping.
1. `submitted` is a nullable timestamp rather than a state enum. It answers the only question that cannot be derived from the members: whether more of them are still coming. How complete a group is, comes from counting the members that exist against `requested`.
1. Members are dismissed in reverse submission order. This makes sure that we do not free up queue before every other job waiting in this group is already dismissed, else it has potential to be picked up.
1. Every member carries a `group:{groupID}` tag, and the prefix is reserved at submission on both endpoints. The tag is what makes a group's jobs findable through the ordinary `/jobs` filter; reserving it stops an unrelated job claiming membership. A group ID is a UUID, so unlike tags generally this one is immune to prefix matching and contains no LIKE wildcard.
1. The group status endpoint applies the rule of the job list rather than of a single job, because it returns many jobs at once: at `AUTH_LEVEL=2` a non-admin sees only the groups they submitted.
1. Group writes on the Database interface are exported while job writes are not. A job writes its own record from inside the jobs package, but a group is submitted from the handlers package, where the process catalog it needs lives.

## Release/Versioning/Changelog

The project uses an automated release workflow triggered by semver tags (e.g., `v1.0.0`, `v1.0.0-beta`). The workflow validates prerequisites, runs security scans, builds multi-platform container images, and creates GitHub releases with auto-generated release notes.

### How to Create a Release

1. **Update CHANGELOG.md**

   - Add a new version entry following the format: `## [X.Y.Z] - YYYY-MM-DD`
   - Document all changes under appropriate categories (API, Features, Configuration, etc.)
   - Add version comparison links at the bottom of the file
   - Release workflow fails if version is missing from CHANGELOG.md
1. **Create and Push a Semver Tag**

   ```bash
   # For a regular release
   git tag v1.0.0
   git push origin v1.0.0

   # For a prerelease (alpha, beta, rc)
   git tag v1.0.0-beta
   git push origin v1.0.0-beta
   ```
1. **Monitor the Release Workflow**

   - The GitHub Actions workflow will automatically trigger
   - It will validate the tag format and CHANGELOG entry
   - Run CodeQL security scan on the codebase
   - Build the container image
   - Run Trivy vulnerability scan on the container
   - Push multi-platform images to GitHub Container Registry
   - Create a GitHub release with release notes copied from CHANGELOG.md
1. **Workflow Can Also Be Triggered Manually**

   - Go to Actions tab → Release workflow → Run workflow
   - Select the tag from the dropdown
   - This is useful for re-running a release if needed