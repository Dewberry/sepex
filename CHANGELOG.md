# Change Log
Notes: During development phase, expect breaking API and YAML schema changes during minor updates. Patch updates are guarenteed to be backward compatible.

## 0.2.0
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

## 0.1.0