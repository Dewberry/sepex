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
- They will be passed to jobs with process id prefix removed. This allow setting 3rd party env variables such as GDAL_NUM_CPUS etc.
- We are parsing at the job level so as to allow dynamic updates without having to restart server

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
- If `"Inputs": {}` in `/execution` payload. Nothing will be appended to process commands. This allow running processes that do not have any inputs.

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


## Release/Versioning/Changelog

The project uses an automated release workflow triggered by semver tags (e.g., `v1.0.0`, `v1.0.0-beta`). The workflow validates prerequisites, runs security scans, builds multi-platform container images, and creates GitHub releases with auto-generated release notes.

### How to Create a Release

1. **Update CHANGELOG.md**
   - Add a new version entry following the format: `## [X.Y.Z] - YYYY-MM-DD`
   - Document all changes under appropriate categories (API, Features, Configuration, etc.)
   - Add version comparison links at the bottom of the file
   - Release workflow fails if version is missing from CHANGELOG.md

2. **Create and Push a Semver Tag**
   ```bash
   # For a regular release
   git tag v1.0.0
   git push origin v1.0.0

   # For a prerelease (alpha, beta, rc)
   git tag v1.0.0-beta
   git push origin v1.0.0-beta
   ```

3. **Monitor the Release Workflow**
   - The GitHub Actions workflow will automatically trigger
   - It will validate the tag format and CHANGELOG entry
   - Run CodeQL security scan on the codebase
   - Build the container image
   - Run Trivy vulnerability scan on the container
   - Push multi-platform images to GitHub Container Registry
   - Create a GitHub release with release notes copied from CHANGELOG.md

4. **Workflow Can Also Be Triggered Manually**
   - Go to Actions tab → Release workflow → Run workflow
   - Select the tag from the dropdown
   - This is useful for re-running a release if needed



