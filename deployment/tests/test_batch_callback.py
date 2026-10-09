import os
import unittest
from unittest.mock import patch

from functions.batch_callback import lambda_function

URLOPEN = "functions.batch_callback.lambda_function.urllib.request.urlopen"


class BatchCallbackTest(unittest.TestCase):
    def test_ignores_unrelated_jobs_and_unhandled_statuses(self) -> None:
        event = {
            "detail": {"jobName": "other_123", "status": "RUNNING"},
            "time": "2026-01-01T00:00:00Z",
        }
        with (
            patch.dict(os.environ, {"API_NAME": "sepex"}),
            patch(URLOPEN) as urlopen,
        ):
            lambda_function.lambda_handler(event, None)
            event["detail"] = {"jobName": "sepex_123", "status": "PENDING"}
            lambda_function.lambda_handler(event, None)
        urlopen.assert_not_called()

    def test_forwards_matching_job(self) -> None:
        event = {
            "detail": {"jobName": "sepex_123", "status": "SUCCEEDED"},
            "time": "2026-01-01T00:00:00Z",
        }
        with (
            patch.dict(
                os.environ, {"API_NAME": "sepex", "SEPEX_URL": "http://localhost"}
            ),
            patch(URLOPEN) as urlopen,
        ):
            lambda_function.lambda_handler(event, None)
        request = urlopen.call_args.args[0]
        self.assertEqual(request.full_url, "http://localhost/jobs/123/status")
        self.assertIn(b'"status": "successful"', request.data)


if __name__ == "__main__":
    unittest.main()
