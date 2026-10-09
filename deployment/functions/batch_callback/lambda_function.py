import json
import os
import urllib.request


def map_batch_status_to_ogc_status(batch_status):
    """Map AWS Batch job statuses to OGC statuses."""
    status_map = {
        "RUNNING": "running",
        "SUCCEEDED": "successful",
        "FAILED": "failed",
    }
    return status_map[batch_status]


def lambda_handler(event, _):
    """Receive a Batch state change event from EventBridge and forward the
    status update to the SEPEX API.

    EventBridge event shape:
        event["detail"]["jobName"]  - SEPEX job name ({api_name}_{jobID})
        event["detail"]["status"]   - Batch status (RUNNING, SUCCEEDED, FAILED)
        event["time"]               - ISO 8601 timestamp
    """
    job_name = event["detail"]["jobName"]
    status = event["detail"]["status"]
    event_time = event["time"]

    if not job_name.startswith(f"{os.environ['API_NAME']}_") or status not in (
        "RUNNING",
        "SUCCEEDED",
        "FAILED",
    ):
        return

    job_id = job_name.rpartition("_")[2]
    url = f"{os.environ['SEPEX_URL']}/jobs/{job_id}/status"

    ogc_status = map_batch_status_to_ogc_status(status)
    payload = {"status": ogc_status, "updated": event_time}

    print(f"sending request to {url} with payload: {payload}")
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, method="PUT")
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=5) as resp:
        print(f"request complete, status: {resp.status}")
