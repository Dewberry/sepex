#!/usr/bin/env python3
"""Register a process YAML with a SEPEX API, or submit a test job to it.

Examples (with scripts/tunnel.sh running):
    register_process.py path/to/process.yml [--update]
    register_process.py test path/to/process.yml --inputs '{"text": "hello"}'
"""

import argparse
import json
import os
import sys
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, urlopen

import yaml


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv or argv[0] not in {"register", "test", "-h", "--help"}:
        argv.insert(0, "register")

    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    register_parser = commands.add_parser(
        "register", help="register or update the process"
    )
    test_parser = commands.add_parser("test", help="submit a smoke-test job")

    for command_parser in (register_parser, test_parser):
        command_parser.add_argument(
            "config",
            type=Path,
            help="process YAML file",
        )
        command_parser.add_argument(
            "--url",
            default=os.environ.get("SEPEX_URL", "http://localhost:8080"),
            help="SEPEX base URL (default: SEPEX_URL or the tunnel.sh address http://localhost:8080)",
        )
        command_parser.add_argument(
            "--token",
            default=os.environ.get("SEPEX_TOKEN"),
            help="Bearer token for an authenticated SEPEX instance (or set SEPEX_TOKEN)",
        )

    register_parser.add_argument(
        "--update",
        action="store_true",
        help="update an existing process with PUT instead of creating one with POST",
    )
    test_parser.add_argument(
        "--inputs",
        default='{"text":"SEPEX smoke test"}',
        help='JSON object of process inputs (default: {"text":"SEPEX smoke test"})',
    )
    args = parser.parse_args(argv)

    if not args.url:
        parser.error("provide --url or set SEPEX_URL")

    try:
        with args.config.open(encoding="utf-8") as process_file:
            process = yaml.safe_load(process_file)
    except OSError as error:
        parser.error(f"cannot read {args.config}: {error}")
    except yaml.YAMLError as error:
        parser.error(f"invalid YAML in {args.config}: {error}")

    if not isinstance(process, dict):
        parser.error("process YAML must contain a mapping at the document root")
    info = process.get("info")
    if not isinstance(info, dict):
        parser.error("process YAML must define info as a mapping")
    process_id = info.get("id")
    if not isinstance(process_id, str) or not process_id:
        parser.error("process YAML must define a non-empty info.id")

    process_url = f"{args.url.rstrip('/')}/processes/{quote(process_id, safe='')}"
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    if args.token:
        headers["Authorization"] = f"Bearer {args.token}"

    if args.command == "test":
        try:
            inputs = json.loads(args.inputs)
        except json.JSONDecodeError as error:
            parser.error(f"--inputs must be a valid JSON object: {error}")
        if not isinstance(inputs, dict):
            parser.error("--inputs must be a JSON object")
        url = f"{process_url}/execution"
        body = {"inputs": inputs}
        method = "POST"
    else:
        url = process_url
        body = process
        method = "PUT" if args.update else "POST"

    request = Request(
        url,
        data=json.dumps(body).encode("utf-8"),
        headers=headers,
        method=method,
    )
    try:
        with urlopen(request, timeout=30) as response:
            body = response.read().decode("utf-8")
            print(body or f"Registered {process_id} (HTTP {response.status})")
    except HTTPError as error:
        body = error.read().decode("utf-8", errors="replace")
        print(f"SEPEX returned HTTP {error.code}: {body}", file=sys.stderr)
        return 1
    except URLError as error:
        print(f"Could not reach SEPEX: {error.reason}", file=sys.stderr)
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
