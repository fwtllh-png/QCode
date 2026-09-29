#!/usr/bin/env python3
"""Exercise Web RC backup/restore and one-release binary downgrade."""

from __future__ import annotations

import argparse
import hashlib
import json
import queue
import shutil
import signal
import sqlite3
import subprocess
import tempfile
import threading
import time
import urllib.request
from pathlib import Path


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--current-binary", required=True)
    parser.add_argument("--previous-binary", required=True)
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--fixture", required=True)
    parser.add_argument("--report", required=True)
    return parser.parse_args()


def wait_for_url(process: subprocess.Popen[str]) -> str:
    lines: queue.Queue[str] = queue.Queue()

    def read_stdout() -> None:
        assert process.stdout is not None
        for line in process.stdout:
            lines.put(line)

    threading.Thread(target=read_stdout, daemon=True).start()
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError("current Web binary exited before readiness")
        try:
            line = lines.get(timeout=0.1)
        except queue.Empty:
            continue
        prefix = "QCode Runtime Ready: "
        if line.startswith(prefix):
            return line[len(prefix) :].strip()
    raise RuntimeError("current Web binary did not become ready")


def request(url: str, token: str, route: str, body: dict, key: str = "") -> dict:
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
    }
    if key:
        headers["Idempotency-Key"] = key
    value = urllib.request.Request(
        url + "api/v1/" + route,
        data=json.dumps(body).encode(),
        headers=headers,
        method="POST",
    )
    with urllib.request.urlopen(value, timeout=10) as response:
        envelope = json.load(response)
    if "problem" in envelope:
        raise RuntimeError(f"{route} failed: {envelope['problem'].get('code')}")
    return envelope["result"]


def bootstrap(url: str) -> str:
    with urllib.request.urlopen(url + "api/v1/bootstrap", timeout=10) as response:
        return json.load(response)["token"]


def stop(process: subprocess.Popen[str]) -> None:
    if process.poll() is not None:
        return
    process.send_signal(signal.SIGINT)
    try:
        process.wait(timeout=15)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)


def file_manifest(root: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for path in sorted(root.rglob("*")):
        if path.is_symlink():
            raise RuntimeError(f"backup source contains symlink: {path}")
        if not path.is_file() or path.name.endswith(".lock"):
            continue
        result[str(path.relative_to(root))] = hashlib.sha256(path.read_bytes()).hexdigest()
    return result


def run_previous(
    binary: str,
    config: str,
    workspace: str,
    fixture: str,
    data_dir: str,
    session_id: str,
    source_turn_id: str,
) -> tuple[int, int, str]:
    previous = subprocess.Popen(
        [
            binary,
            "--config",
            config,
            "--data-dir",
            data_dir,
            "--provider-fixture",
            fixture,
            "--port",
            "0",
        ],
        cwd=workspace,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    try:
        url = wait_for_url(previous)
        token = bootstrap(url)
        sessions = request(url, token, "session/list", {"limit": 20})["sessions"]
        if session_id not in {item["session_id"] for item in sessions}:
            raise RuntimeError("previous binary could not list the Web RC session")
        status = request(url, token, "session/status", {"session_id": session_id})
        history = request(
            url,
            token,
            "session/history",
            {"session_id": session_id, "limit": 256},
        )
        latest = int(status["latest_sequence"])
        events = history["events"]
        source_events = [
            event for event in events if event.get("turn_id") == source_turn_id
        ]
        terminal_kinds = {"turn.completed", "turn.failed", "turn.canceled"}
        if (
            latest < 1
            or not source_events
            or not any(event.get("kind") in terminal_kinds for event in source_events)
        ):
            raise RuntimeError("previous binary could not recover the Web RC Turn")
        recovery = request(
            url,
            token,
            "turn/recover",
            {
                "version": 1,
                "session_id": session_id,
                "source_turn_id": source_turn_id,
                "action": "retry",
                "idempotency_key": "release-drill-recover",
            },
        )
        if not recovery.get("accepted") or not recovery.get("turn_id"):
            raise RuntimeError("previous binary did not accept Turn recovery")
        return latest, len(events), str(recovery["turn_id"])
    finally:
        stop(previous)


def main() -> None:
    args = parse_args()
    workspace = str(Path(args.workspace).resolve())
    with tempfile.TemporaryDirectory(prefix="qcode-release-drill-") as temp:
        root = Path(temp)
        data = root / "data"
        config = root / "runtime.toml"
        config.write_text(
            "[execution]\n"
            f"workspace = {json.dumps(workspace)}\n"
            'provider = "openai"\nmodel = "fixture-model"\ntools = true\n'
        )
        current = subprocess.Popen(
            [
                args.current_binary,
                "--config",
                str(config),
                "--data-dir",
                str(data),
                "--provider-fixture",
                args.fixture,
                "--port",
                "0",
            ],
            cwd=workspace,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        try:
            url = wait_for_url(current)
            token = bootstrap(url)
            binding = request(
                url,
                token,
                "session/create",
                {
                    "session_id": "session_release_drill",
                    "title": "Downgrade Drill",
                    "isolation": "shared",
                },
                "release-drill-create",
            )
            replayed = request(
                url,
                token,
                "session/create",
                {
                    "session_id": "session_release_drill",
                    "title": "Downgrade Drill",
                    "isolation": "shared",
                },
                "release-drill-create",
            )
            if replayed != binding:
                raise RuntimeError("Web RC session create retry changed its binding")
            session_id = binding["session_id"]
            submitted = request(
                url,
                token,
                "operation/submit",
                {
                    "session_id": session_id,
                    "kind": "turn.start",
                    "idempotency_key": "release-drill-turn",
                    "payload": {"prompt": "say hello"},
                },
            )
            source_turn_id = submitted["turn_id"]
            for _ in range(100):
                status = request(
                    url, token, "session/status", {"session_id": session_id}
                )["status"]
                if status == "completed":
                    break
                time.sleep(0.1)
            else:
                raise RuntimeError("Web RC turn did not complete")
        finally:
            stop(current)

        backup = root / "backup"
        restored = root / "restored"
        shutil.copytree(data, backup, ignore=shutil.ignore_patterns("*.lock"))
        source_manifest = file_manifest(backup)
        shutil.copytree(backup, restored)
        restored_manifest = file_manifest(restored)
        if source_manifest != restored_manifest:
            raise RuntimeError("restored data directory digest differs from backup")

        database_path = restored / "state-v1.db"
        if not database_path.is_file():
            raise RuntimeError("restored data directory has no state database")
        database = sqlite3.connect(str(database_path))
        try:
            database.execute("PRAGMA query_only = ON")
            schema_version = database.execute("PRAGMA user_version").fetchone()[0]
            watermark = database.execute(
                "SELECT COALESCE(MAX(sequence), 0) FROM event_reservations"
            ).fetchone()[0]
        finally:
            database.close()
        latest, event_count, recovery_turn_id = run_previous(
            args.previous_binary,
            str(config),
            workspace,
            args.fixture,
            str(restored),
            session_id,
            source_turn_id,
        )
        report = {
            "version": 1,
            "status": "passed",
            "current_binary_sha256": hashlib.sha256(
                Path(args.current_binary).read_bytes()
            ).hexdigest(),
            "previous_binary_sha256": hashlib.sha256(
                Path(args.previous_binary).read_bytes()
            ).hexdigest(),
            "schema_version": schema_version,
            "event_watermark": watermark,
            "previous_binary_latest_sequence": latest,
            "previous_binary_event_count": event_count,
            "source_turn_id": source_turn_id,
            "recovery_turn_id": recovery_turn_id,
            "recovery_accepted": True,
            "session_id": session_id,
            "backup_file_count": len(source_manifest),
            "backup_digest": hashlib.sha256(
                json.dumps(source_manifest, sort_keys=True).encode()
            ).hexdigest(),
        }
        report_path = Path(args.report)
        report_path.parent.mkdir(parents=True, exist_ok=True)
        report_path.write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps(report, sort_keys=True))


if __name__ == "__main__":
    main()
