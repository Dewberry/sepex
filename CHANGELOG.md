# Changelog

All notable changes to this project will be documented in this file.

This project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> [!IMPORTANT]
> Major version zero (0.y.z) is for initial development. During initial development phase, expect breaking API and YAML schema changes during minor updates. Patch updates are guaranteed to be backward compatible during this phase.]

## Unreleased

### API
#### GET /admin/resources
- New endpoint to view resource utilization for local jobs (docker, subprocess) and queue status

### Configuration
- New `MAX_LOCAL_CPUS` and `MAX_LOCAL_MEMORY` environment variables (or `--max-local-cpus` and `--max-local-memory` CLI flags) to set resource limits for local job scheduling
- Process definitions are validated against these limits at startup and when adding/updating processes via API
- Processes without explicit resource requirements use default values

### Documentation
- Added sequence diagram for local scheduler

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

[Unreleased]: https://github.com/Dewberry/sepex/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/Dewberry/sepex/releases/tag/v0.2.1

[0.2.0]: https://github.com/Dewberry/sepex/releases/tag/v0.2.0
[0.1.0]: https://github.com/Dewberry/sepex/releases/tag/v0.1.0