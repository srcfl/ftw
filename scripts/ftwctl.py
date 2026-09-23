#!/usr/bin/env python3
"""Visible backup and native update commands for an FTW box."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import queue
import re
import shlex
import socket
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from urllib import error, parse, request


class FTWError(Exception):
    pass


def say(message: str) -> None:
    print(message, flush=True)


def elapsed(start: float) -> str:
    seconds = int(time.monotonic() - start)
    return f"{seconds // 60}m {seconds % 60:02d}s"


def size(value: int) -> str:
    if value >= 1024**3:
        return f"{value / 1024**3:.2f} GiB"
    if value >= 1024**2:
        return f"{value / 1024**2:.1f} MiB"
    if value >= 1024:
        return f"{value / 1024:.1f} KiB"
    return f"{value} B"


class API:
    def __init__(self, base: str, token: str = "") -> None:
        parsed = parse.urlsplit(base)
        if (parsed.scheme not in {"http", "https"} or not parsed.netloc or
                parsed.path not in {"", "/"} or parsed.query or parsed.fragment or
                parsed.username or parsed.password):
            raise FTWError("--url must be an HTTP(S) origin, without a path")
        if token and parsed.scheme == "http" and parsed.hostname not in {"localhost", "127.0.0.1", "::1"}:
            raise FTWError("use HTTPS or an SSH tunnel to localhost when FTW_API_TOKEN is set")
        self.base = base.rstrip("/")
        self.token = token

    def open(self, method: str, path: str, body: dict | None = None, timeout: int = 15):
        data = json.dumps(body).encode() if body is not None else None
        headers = {"Accept": "application/json"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        req = request.Request(self.base + path, data=data, headers=headers, method=method)
        try:
            return request.urlopen(req, timeout=timeout)
        except error.HTTPError as exc:
            detail = exc.read(2048).decode("utf-8", "replace")
            try:
                detail = json.loads(detail).get("error", detail)
            except ValueError:
                pass
            raise FTWError(f"{method} {path}: HTTP {exc.code}: {detail}") from exc
        except error.URLError as exc:
            raise FTWError(f"{method} {path}: {exc.reason}") from exc

    def json(self, method: str, path: str, body: dict | None = None, timeout: int = 15) -> dict:
        with self.open(method, path, body, timeout) as response:
            return json.load(response)


def show_progress(progress: dict, start: float, prefix: str) -> str:
    phase = progress.get("phase") or progress.get("state") or "waiting"
    step = ""
    if progress.get("step") and progress.get("total_steps"):
        step = f" step {progress['step']}/{progress['total_steps']}"
    done = int(progress.get("completed_bytes") or progress.get("progress_current") or 0)
    total = int(progress.get("total_bytes") or progress.get("progress_total") or 0)
    amount = f" {size(done)}/{size(total)} ({done * 100 // total}%)" if total else (f" {size(done)}; total unknown" if done else " total unknown")
    rows = f" {progress['table']}: {progress['rows_done']} rows; total unknown" if progress.get("table") and progress.get("rows_done") else ""
    message = progress.get("message") or progress.get("error") or ""
    return f"[{elapsed(start)}] {prefix}: {phase}{step}{amount}{rows}{': ' + message if message else ''}"


def watch(get_progress, running, start: float, prefix: str, interval: float = 5.0, heartbeat: float = 10.0) -> None:
    last_key = None
    last_print = 0.0
    last = {}
    while running():
        try:
            last = get_progress()
        except FTWError as exc:
            last = {"phase": "temporarily unavailable", "message": str(exc)}
        key = json.dumps(last, sort_keys=True)
        now = time.monotonic()
        if key != last_key or now - last_print >= heartbeat:
            say(show_progress(last, start, prefix))
            last_key, last_print = key, now
        time.sleep(interval)
    try:
        final = get_progress()
    except FTWError:
        final = last
    if final:
        say(show_progress(final, start, prefix))


def status(api: API) -> None:
    health = api.json("GET", "/api/health")
    say(f"Core health: {health.get('status', 'unknown')}; drivers ok/offline/faulted: "
        f"{health.get('drivers_ok', '?')}/{health.get('drivers_offline', '?')}/{health.get('drivers_faulted', '?')}")
    history = health.get("history_storage") or {}
    if history:
        say(f"History: {history.get('engine', '?')}; migration {history.get('migration', {}).get('state', '?')}; "
            f"maintenance {history.get('maintenance', {}).get('state', '?')}")
    info = core_identity(api)
    say(f"Version: {info.get('current', '?')}; channel: {info.get('channel', '?')}; "
        f"native: {bool(info.get('native'))}; available: {info.get('latest', 'none') if info.get('update_available') else 'none'}")
    try:
        update = api.json("GET", "/api/version/update/status")
        say(f"Update: {update.get('state', 'idle')}; {update.get('message', '')}")
    except FTWError as exc:
        say(f"Update status unavailable: {exc}")
    try:
        backups = api.json("GET", "/api/backups")
        latest = backups.get("backups", [])[:1]
        say(f"Full backup on box: {latest[0]['id'] if latest else 'none'}; "
            f"current backup phase: {backups.get('progress', {}).get('phase') or 'idle'}")
    except FTWError as exc:
        say(f"Backup status unavailable: {exc}")


def core_identity(api: API) -> dict:
    try:
        return api.json("GET", "/api/version/check")
    except FTWError as exc:
        if "HTTP 503: self-update disabled" not in str(exc):
            raise
        site = api.json("GET", "/api/status")
        return {"current": site.get("version", ""), "native": False, "channel": "old line"}


def download_backup(api: API, entry: dict, output_dir: Path, start: float) -> Path:
    backup_id = entry.get("id", "")
    if not backup_id.startswith("ftw-full-backup-") or not backup_id.endswith(".ftwbak") or "/" in backup_id:
        raise FTWError("server returned an invalid backup name")
    expected = entry.get("sha256", "")
    if len(expected) != 64 or any(c not in "0123456789abcdef" for c in expected):
        raise FTWError("server did not provide a verified SHA-256 digest")
    if not entry.get("verified"):
        raise FTWError("server has not verified the backup")
    output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    target = output_dir / backup_id
    pending = output_dir / (backup_id + ".part")
    if target.exists() or pending.exists():
        raise FTWError(f"backup target already exists: {target}")
    digest = hashlib.sha256()
    copied = 0
    last_print = 0.0
    try:
        with api.open("GET", "/api/backups/" + parse.quote(backup_id), timeout=3600) as source:
            total = int(source.headers.get("Content-Length") or 0)
            fd = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "wb") as dest:
                while chunk := source.read(1024 * 1024):
                    dest.write(chunk)
                    digest.update(chunk)
                    copied += len(chunk)
                    if time.monotonic() - last_print >= 2:
                        say(show_progress({"phase": "copying_off_box", "completed_bytes": copied, "total_bytes": total}, start, "backup"))
                        last_print = time.monotonic()
                dest.flush()
                os.fsync(dest.fileno())
        if digest.hexdigest() != expected or (entry.get("size_bytes") and copied != entry["size_bytes"]):
            raise FTWError("downloaded backup does not match the server's verified size and SHA-256")
        os.link(pending, target)
        pending.unlink()
    except BaseException:
        pending.unlink(missing_ok=True)
        raise
    say(f"Backup saved on this computer: {target} ({size(copied)}, SHA-256 {expected})")
    return target


def backup(api: API, output_dir: Path) -> Path:
    start = time.monotonic()
    say("Starting full backup on box. The current Core stays active.")
    result: queue.Queue = queue.Queue(maxsize=1)

    def create() -> None:
        try:
            result.put((api.json("POST", "/api/backups", {}, timeout=7200), None))
        except Exception as exc:
            result.put((None, exc))

    threading.Thread(target=create, daemon=True).start()
    watch(lambda: api.json("GET", "/api/backups").get("progress", {}), result.empty, start, "backup")
    response, exc = result.get()
    if exc:
        raise exc
    entry = response.get("backup", {})
    if response.get("warning"):
        say(f"Backup warning: {response['warning']}")
    say(f"Archive verified on box: {entry.get('id', '?')}")
    return download_backup(api, entry, output_dir, start)


def update(api: API, channel: str | None, backup_dir: Path | None, max_wait: int) -> None:
    if max_wait < 60:
        raise FTWError("--max-wait must be at least 60 seconds")
    current = core_identity(api)
    if not current.get("native"):
        raise FTWError("this is an old installation; its Update button cannot cross to native 0.x")
    if channel:
        api.json("POST", "/api/version/channel", {"channel": channel})
    info = api.json("GET", "/api/version/check?force=1", timeout=30)
    if info.get("err"):
        raise FTWError(f"release check failed: {info['err']}")
    if not info.get("update_available"):
        say(f"Already current on {info.get('channel', '?')}: {info.get('current', '?')}")
        return
    if info.get("full_backup_required") and backup_dir is None:
        raise FTWError("this release changes stored data; pass --backup-dir to save a verified copy off box first")
    if backup_dir is not None:
        backup(api, backup_dir)
    target = info["latest"]
    say(f"Updating {info.get('current')} -> {target} on {info.get('channel')}.")
    say("Core creates its local rollback point before downloading; progress follows below.")
    response = api.json("POST", "/api/version/update", {}, timeout=30)
    if response.get("target") != target:
        raise FTWError("Core accepted a different update target")
    start = time.monotonic()
    last_key = None
    last_print = 0.0
    while time.monotonic() - start < max_wait:
        try:
            progress = api.json("GET", "/api/version/update/status")
        except FTWError as exc:
            progress = {"state": "restarting", "message": f"API unavailable during restart: {exc}"}
        key = json.dumps(progress, sort_keys=True)
        now = time.monotonic()
        if key != last_key or now - last_print >= 10:
            say(show_progress(progress, start, "update"))
            last_key, last_print = key, now
        if progress.get("state") == "failed":
            raise FTWError(f"update failed: {progress.get('message', 'see Core logs')}")
        if progress.get("state") == "done":
            health = api.json("GET", "/api/health")
            current = api.json("GET", "/api/version/check").get("current")
            if current != target or health.get("status") != "ok":
                raise FTWError(f"update ended, but version/health differs: {current}, {health.get('status')}")
            say(f"Update complete: {current}; health ok; {elapsed(start)}")
            return
        time.sleep(2)
    raise FTWError(f"update did not finish within {max_wait}s; Core may still be working; inspect status")


def remote(host: str, *args: str, input_data: bytes | None = None) -> str:
    if not re.fullmatch(r"[A-Za-z0-9._@-]+", host) or host.startswith("-"):
        raise FTWError("invalid SSH host")
    command = " ".join(shlex.quote(str(arg)) for arg in args)
    process = subprocess.Popen(
        ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", host, command],
        stdin=subprocess.PIPE if input_data is not None else subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    start = time.monotonic()
    while True:
        try:
            output, stderr = process.communicate(input=input_data, timeout=10)
            break
        except subprocess.TimeoutExpired:
            input_data = None
            say(f"[{elapsed(start)}] SSH step still running: {args[0]} {' '.join(args[1:3])}")
            if time.monotonic() - start >= 3600:
                process.kill()
                process.communicate()
                raise FTWError(f"SSH step timed out: {args[0]}")
    if process.returncode:
        detail = stderr.decode("utf-8", "replace").strip()
        raise FTWError(f"SSH {command}: {detail or 'exit ' + str(process.returncode)}")
    return output.decode("utf-8", "replace").strip()


def remote_copy(host: str, source: Path, target: str, label: str) -> None:
    command = "umask 077 && cat > " + shlex.quote(target)
    process = subprocess.Popen(
        ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", host, command],
        stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
    )
    total = source.stat().st_size
    done = 0
    start = time.monotonic()
    last_print = 0.0
    try:
        with source.open("rb") as stream:
            while chunk := stream.read(1024 * 1024):
                process.stdin.write(chunk)
                done += len(chunk)
                if time.monotonic() - last_print >= 2:
                    say(show_progress({"phase": label, "completed_bytes": done, "total_bytes": total}, start, "migration"))
                    last_print = time.monotonic()
        process.stdin.close()
        if process.wait() != 0:
            raise FTWError(f"copy to {host} failed: {process.stderr.read().decode('utf-8', 'replace').strip()}")
    except BaseException:
        process.kill()
        process.wait()
        raise
    say(show_progress({"phase": label, "completed_bytes": done, "total_bytes": total}, start, "migration"))


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def verified_backup(api: API, path: Path) -> tuple[str, str]:
    if not path.is_file() or not path.name.endswith(".ftwbak"):
        raise FTWError("--backup must name a downloaded .ftwbak file")
    entries = api.json("GET", "/api/backups").get("backups", [])
    entry = next((item for item in entries if item.get("id") == path.name), None)
    if not entry or not entry.get("verified"):
        raise FTWError("the running site has no matching verified backup")
    created = entry.get("created_at", "")
    try:
        from datetime import datetime, timezone
        age = (datetime.now(timezone.utc) - datetime.fromisoformat(created.replace("Z", "+00:00"))).total_seconds()
    except ValueError as exc:
        raise FTWError("backup has no valid creation time") from exc
    if age < -300 or age > 24 * 3600:
        raise FTWError("migration needs a full backup from the last 24 hours")
    digest = sha256_file(path)
    if digest != entry.get("sha256") or path.stat().st_size != entry.get("size_bytes"):
        raise FTWError("local backup bytes do not match the site's verified backup")
    return path.name, digest


def published_package(tag: str, arch: str, work: Path) -> tuple[Path, Path, Path]:
    name = f"ftw-linux-{arch}.tar.gz"
    base = f"https://github.com/srcfl/ftw/releases/download/{tag}/"
    archive, checksum, launcher = work / name, work / (name + ".sha256"), work / "ftw-launcher"
    for url, target in ((base + name + ".sha256", checksum), (base + name, archive)):
        say(f"Downloading {target.name} from exact tag {tag}")
        req = request.Request(url, headers={"User-Agent": "ftwctl-native-migration"})
        try:
            with request.urlopen(req, timeout=120) as source, target.open("xb") as output:
                total = int(source.headers.get("Content-Length") or 0)
                done, start, last = 0, time.monotonic(), 0.0
                while chunk := source.read(1024 * 1024):
                    output.write(chunk)
                    done += len(chunk)
                    if time.monotonic() - last >= 2:
                        say(show_progress({"phase": "downloading_package", "completed_bytes": done, "total_bytes": total}, start, "migration"))
                        last = time.monotonic()
        except error.URLError as exc:
            raise FTWError(f"release download failed: {exc.reason}") from exc
    checksum_line = checksum.read_text().strip()
    match = re.fullmatch(rf"([0-9a-f]{{64}})  {re.escape(name)}", checksum_line)
    if not match or sha256_file(archive) != match.group(1):
        raise FTWError("published package checksum does not match")
    try:
        with tarfile.open(archive, "r:gz") as tar:
            member = tar.getmember("ftw-launcher")
            if not member.isfile() or member.size > 30 * 1024 * 1024:
                raise FTWError("package has no safe launcher")
            with tar.extractfile(member) as source, launcher.open("xb") as dest:
                while chunk := source.read(1024 * 1024):
                    dest.write(chunk)
    except (KeyError, tarfile.TarError) as exc:
        raise FTWError("published package has no readable launcher") from exc
    launcher.chmod(0o755)
    say(f"Verified package: {name}, SHA-256 {match.group(1)}")
    return archive, checksum, launcher


def wait_for_version(api: API, wanted: str, min_drivers: int, timeout: int) -> bool:
    start = time.monotonic()
    last = 0.0
    while time.monotonic() - start < timeout:
        try:
            health = api.json("GET", "/api/health", timeout=5)
            info = core_identity(api)
            current = info.get("current")
            drivers = int(health.get("drivers_ok") or 0)
            history_state = health.get("history_storage", {}).get("migration", {}).get("state", "unknown")
            history_ready = not wanted.startswith("v0.") or history_state == "complete"
            if current == wanted and health.get("status") == "ok" and drivers >= min_drivers and history_ready:
                say(f"Ready: {current}; health ok; {drivers} drivers ok; history {history_state}; {elapsed(start)}")
                return True
            detail = f"version={current}, health={health.get('status')}, drivers={drivers}/{min_drivers}, history={history_state}"
        except FTWError as exc:
            detail = f"API not ready: {exc}"
        if time.monotonic() - last >= 10:
            say(f"[{elapsed(start)}] Waiting for Core: {detail}")
            last = time.monotonic()
        time.sleep(2)
    return False


def migrate_native(api: API, args) -> None:
    if args.max_wait < 60:
        raise FTWError("--max-wait must be at least 60 seconds")
    tag = re.fullmatch(r"v0\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-beta\.([1-9][0-9]*))?", args.tag)
    if not tag or int(tag.group(1)) < 131:
        raise FTWError("migration needs an exact native tag v0.131.0 or later")
    safe_path = re.compile(r"/[A-Za-z0-9_./-]+")
    paths = (args.root, args.data_dir, args.config, args.user_drivers)
    if (not all(safe_path.fullmatch(path) and os.path.normpath(path) == path for path in paths) or
            not args.root.startswith("/opt/")):
        raise FTWError("root, data dir and config must be absolute paths")
    if not re.fullmatch(r"[A-Za-z0-9_.@-]+\.service", args.unit):
        raise FTWError("invalid systemd unit")
    host_name = remote(args.host, "hostname", "-f")
    if host_name == socket.getfqdn():
        raise FTWError("run migration from another computer so the verified backup is off box")
    info = core_identity(api)
    health = api.json("GET", "/api/health")
    if info.get("native") or not re.fullmatch(r"v[123]\.[0-9]+\.[0-9]+(?:-beta\.[0-9]+)?", info.get("current", "")):
        raise FTWError("migrate-native only accepts an older 1.x, 2.x or 3.x Core")
    if health.get("status") != "ok":
        raise FTWError("old Core is not healthy; investigate before migration")
    if remote(args.host, "systemctl", "is-active", args.unit) != "active":
        raise FTWError("old systemd service is not active")
    host_status = json.loads(remote(args.host, "curl", "-fsS", "--max-time", "5", "http://127.0.0.1:8080/api/status"))
    if host_status.get("version") != info.get("current"):
        raise FTWError("--url and --host report different Core versions; refusing to mix sites")
    unit = remote(args.host, "systemctl", "show", args.unit, "-p", "ExecStart", "-p", "User", "-p", "Group", "-p", "BindPaths", "--no-pager")
    if "User=ftw" not in unit or "Group=ftw" not in unit or not re.search(r"path=/opt/ftw/ftw(?:\s|;)", unit) or "ftw-launcher" in unit or "docker" in unit.lower():
        raise FTWError("this service is not the supported older native/systemd layout; Docker stays untouched")
    if args.data_dir not in unit and not args.config.startswith(args.data_dir + "/"):
        raise FTWError("data directory does not match the unit's bind mount or config path")
    remote(args.host, "sudo", "-n", "true")
    remote(args.host, "sudo", "-n", "test", "-f", args.data_dir + "/state.db")
    override = f"/etc/systemd/system/{args.unit}.d/zz-native-migration.conf"
    if args.check_only:
        say(f"Preflight ok: {info['current']} on {args.unit}, {health.get('drivers_ok', 0)} drivers ok")
        say(f"Data: {args.data_dir}; config in service: {args.config}; native root: {args.root}")
        say("No service or data changed. Migration still needs a new verified backup off box.")
        return
    if args.backup is None:
        raise FTWError("run backup --output-dir on this computer, then pass --backup to migrate-native")
    backup_name, backup_sha = verified_backup(api, args.backup)
    remote(args.host, "sudo", "-n", "test", "!", "-e", args.root)
    remote(args.host, "sudo", "-n", "test", "!", "-L", args.root)
    remote(args.host, "sudo", "-n", "test", "!", "-e", override)
    remote(args.host, "sudo", "-n", "test", "!", "-L", override)
    arch = remote(args.host, "uname", "-m")
    arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(arch)
    if arch is None:
        raise FTWError("native release supports only ARM64 and AMD64")
    baseline = int(health.get("drivers_ok") or 0)
    before_drivers = set(api.json("GET", "/api/drivers"))
    identity_path = args.data_dir + "/nova.key"
    try:
        remote(args.host, "sudo", "-n", "test", "-f", identity_path)
        identity_sha = remote(args.host, "sudo", "-n", "sha256sum", identity_path).split()[0]
    except FTWError:
        identity_sha = None
    say(f"Migrating {info['current']} -> {args.tag}; old service remains active until the verified package and backup are ready.")
    with tempfile.TemporaryDirectory(prefix="ftw-migrate-") as work_name:
        work = Path(work_name)
        archive, checksum, launcher = published_package(args.tag, arch, work)
        remote_dir = remote(args.host, "mktemp", "-d", "/tmp/ftw-migrate.XXXXXXXX")
        if not re.fullmatch(r"/tmp/ftw-migrate\.[A-Za-z0-9]+", remote_dir):
            raise FTWError("SSH host returned an unexpected staging directory")
        say(f"Recovery copy on box during migration: {remote_dir}/{backup_name}")
        remote(args.host, "sudo", "-n", "chgrp", "ftw", remote_dir)
        remote(args.host, "sudo", "-n", "chmod", "0750", remote_dir)
        for source in (archive, checksum, launcher, args.backup):
            remote_copy(args.host, source, remote_dir + "/" + source.name, "copying_" + source.name)
        if remote(args.host, "sha256sum", remote_dir + "/" + backup_name).split()[0] != backup_sha:
            raise FTWError("recovery copy changed in transit")
        for source in (archive, checksum):
            remote(args.host, "sudo", "-n", "chgrp", "ftw", remote_dir + "/" + source.name)
            remote(args.host, "sudo", "-n", "chmod", "0640", remote_dir + "/" + source.name)
        remote(args.host, "sudo", "-n", "install", "-d", "-o", "ftw", "-g", "ftw", "-m", "0750", args.root)
        remote(args.host, "sudo", "-n", "install", "-o", "ftw", "-g", "ftw", "-m", "0755", remote_dir + "/ftw-launcher", args.root + "/ftw-launcher")
        remote(args.host, "sudo", "-n", "-u", "ftw", args.root + "/ftw-launcher", "-root", args.root, "install", args.tag, remote_dir + "/" + archive.name, remote_dir + "/" + checksum.name)
        remote(args.host, "sudo", "-n", "-u", "ftw", args.root + "/ftw-launcher", "-root", args.root, "init", args.tag)
        slots = json.loads(remote(args.host, "sudo", "-n", "-u", "ftw", args.root + "/ftw-launcher", "-root", args.root, "status"))
        if slots.get("current") != args.tag:
            raise FTWError("installed slot did not select the requested tag")
        say("Package installed in a separate root. Switching the systemd start command now.")
        dropin = ("[Service]\nExecStart=\n" +
            f"ExecStart={args.root}/ftw-launcher -root {args.root} -config {args.config} -user-drivers {args.user_drivers}\n" +
            "Restart=always\n")
        switched = False
        try:
            remote(args.host, "sudo", "-n", "systemctl", "stop", args.unit)
            remote(args.host, "sudo", "-n", "install", "-d", "-m", "0755", f"/etc/systemd/system/{args.unit}.d")
            # tee can write the override and still fail before returning.
            # From this point every failure must remove it before old Core starts.
            switched = True
            remote(args.host, "sudo", "-n", "tee", override, input_data=dropin.encode())
            remote(args.host, "sudo", "-n", "systemctl", "daemon-reload")
            remote(args.host, "sudo", "-n", "systemctl", "start", args.unit)
            if not wait_for_version(api, args.tag, baseline, args.max_wait):
                raise FTWError("native Core did not reach the old site's healthy driver count")
            after_drivers = set(api.json("GET", "/api/drivers"))
            if not before_drivers.issubset(after_drivers):
                raise FTWError("one or more configured drivers are missing after migration")
            if identity_sha and remote(args.host, "sudo", "-n", "sha256sum", identity_path).split()[0] != identity_sha:
                raise FTWError("site identity changed after migration")
        except BaseException as exc:
            say(f"Migration failed: {exc}. Restoring the old start command.")
            if switched:
                remote(args.host, "sudo", "-n", "systemctl", "stop", args.unit)
                remote(args.host, "sudo", "-n", "rm", "-f", override)
                remote(args.host, "sudo", "-n", "systemctl", "daemon-reload")
            remote(args.host, "sudo", "-n", "systemctl", "start", args.unit)
            if not wait_for_version(api, info["current"], baseline, 120):
                say("Old Core did not recover from the binary switch. Restoring verified data while stopped.")
                remote(args.host, "sudo", "-n", "systemctl", "stop", args.unit)
                restored_bin = f"{args.root}/releases/{args.tag}/ftw-backup"
                remote(args.host, "sudo", "-n", restored_bin, "restore", "-archive", remote_dir + "/" + backup_name, "-data", args.data_dir, "-yes")
                remote(args.host, "sudo", "-n", "systemctl", "start", args.unit)
                if not wait_for_version(api, info["current"], baseline, 180):
                    raise FTWError(f"automatic recovery failed; keep {remote_dir}/{backup_name} and inspect {args.unit}") from exc
            raise FTWError("migration rolled back; old Core and data are running") from exc
        say(f"Migration complete: {args.tag}; old binary remains at its original path.")
        say(f"Verified backup remains on this computer: {args.backup}")
        remote(args.host, "sudo", "-n", "rm", "-rf", "--", remote_dir)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:8080", help="box API origin; use an SSH tunnel from another machine if needed")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("status", help="show health, update and backup state")
    backup_args = sub.add_parser("backup", help="create, verify and copy a full backup to this computer")
    backup_args.add_argument("--output-dir", type=Path, required=True)
    update_args = sub.add_parser("update", help="use the native 0.x Update Center API and show every phase")
    update_args.add_argument("--channel", choices=["beta", "stable"])
    update_args.add_argument("--backup-dir", type=Path, help="off-box directory for a required full backup")
    update_args.add_argument("--max-wait", type=int, default=3600)
    migrate_args = sub.add_parser("migrate-native", help="move an older native/systemd site to one exact 0.x release")
    migrate_args.add_argument("--host", required=True, help="SSH name of the box")
    migrate_args.add_argument("--tag", required=True, help="exact published 0.x release")
    migrate_args.add_argument("--backup", type=Path, help="fresh verified full backup on this computer")
    migrate_args.add_argument("--data-dir", required=True, help="box host path to persistent data")
    migrate_args.add_argument("--config", required=True, help="config path as seen by the systemd service")
    migrate_args.add_argument("--user-drivers", required=True, help="driver overlay path as seen by the service")
    migrate_args.add_argument("--root", default="/opt/ftw-native", help="new, empty native release root")
    migrate_args.add_argument("--unit", default="ftw.service")
    migrate_args.add_argument("--check-only", action="store_true", help="inspect the old service without changing it")
    migrate_args.add_argument("--max-wait", type=int, default=1800)
    args = parser.parse_args(argv)
    try:
        api = API(args.url, os.environ.get("FTW_API_TOKEN", ""))
        if args.command == "status":
            status(api)
        elif args.command == "backup":
            backup(api, args.output_dir)
        elif args.command == "update":
            update(api, args.channel, args.backup_dir, args.max_wait)
        elif args.command == "migrate-native":
            migrate_native(api, args)
    except (FTWError, OSError, ValueError) as exc:
        print(f"ftwctl: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
