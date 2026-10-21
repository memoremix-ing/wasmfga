#!/usr/bin/env python3
"""Run the oracle suites, including open gaps, against exact release artifacts."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
from datetime import datetime, timezone


ROOT = Path(__file__).resolve().parent.parent


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--engine", choices=("go", "rust", "both"), default="both")
    parser.add_argument("--run", help="Go test name filter; recorded as partial coverage")
    parser.add_argument("--output", type=Path, default=ROOT / "dist/conformance")
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    report = {
        "startedAt": datetime.now(timezone.utc).isoformat(),
        "upstreamManifest": json.loads((ROOT / "upstream.json").read_text()),
        "scope": "filtered" if args.run else "all tests in ./tests, including known gaps",
        "testFilter": args.run,
        "engines": {},
    }
    for name in ("go", "rust"):
        if args.engine != "both" and args.engine != name:
            continue
        artifact = ROOT / "dist" / ("wasmfga.wasm" if name == "go" else "wasmfga-rust.wasm")
        if not artifact.is_file():
            report["engines"][name] = {"passed": False, "error": f"missing artifact: {artifact}; run make build"}
            continue
        before = sha256(artifact)
        command = ["go", "test", "-json", "-tags", "conformance_audit", "./tests", "-count=1"]
        if args.run:
            command += ["-run", args.run]
        print(f"Conformance: {name} ({before[:12]})", flush=True)
        env = {**os.environ, "GOTOOLCHAIN": "local", "WASMFGA_WASM": str(artifact)}
        log_path = args.output / f"{name}.jsonl"
        failed = []
        skipped = []
        passed = 0
        with log_path.open("w") as log:
            result = subprocess.Popen(command, cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            for line in result.stdout:
                try:
                    event = json.loads(line)
                except json.JSONDecodeError:
                    event = {"Action": "runner_output", "Output": line}
                    print(line, end="", flush=True)
                log.write(json.dumps(event) + "\n")
                if event.get("Action") == "fail" and event.get("Test"):
                    failed.append(event["Test"])
                    print(f"  FAIL {event['Test']}", flush=True)
                if event.get("Action") == "pass" and event.get("Test"):
                    passed += 1
                if event.get("Action") == "skip" and event.get("Test"):
                    skipped.append(event["Test"])
            code = result.wait()
        unchanged = artifact.is_file() and sha256(artifact) == before
        report["engines"][name] = {
            "passed": code == 0 and unchanged and passed > 0,
            "artifact": str(artifact), "sha256": before,
            "artifactUnchanged": unchanged, "exitCode": code,
            "command": command, "passedTestEvents": passed,
            "failedTests": failed, "skippedTests": skipped, "log": str(log_path.resolve()),
        }
    report["completedAt"] = datetime.now(timezone.utc).isoformat()
    report["passed"] = all(e["passed"] for e in report["engines"].values())
    report_path = args.output / "report.json"
    report_path.write_text(json.dumps(report, indent=2) + "\n")
    print(f"{'PASS' if report['passed'] else 'FAIL'}: {report_path.resolve()}", flush=True)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
