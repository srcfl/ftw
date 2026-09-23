import hashlib
import importlib.util
import io
from contextlib import redirect_stdout
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location("ftwctl", Path(__file__).with_name("ftwctl.py"))
ftwctl = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ftwctl)


class FakeAPI:
    def __init__(self, responses):
        self.responses = responses
        self.calls = []

    def json(self, method, path, body=None, timeout=15):
        self.calls.append((method, path))
        answer = self.responses[(method, path)]
        if isinstance(answer, Exception):
            raise answer
        return answer


class BackupResponse(io.BytesIO):
    def __init__(self, payload):
        super().__init__(payload)
        self.headers = {"Content-Length": str(len(payload))}


class BackupAPI:
    def __init__(self, payload):
        self.payload = payload

    def open(self, method, path, timeout=15):
        return BackupResponse(self.payload)


class FTWCTLTests(unittest.TestCase):
    def test_token_requires_private_tunnel_or_https(self):
        with self.assertRaisesRegex(ftwctl.FTWError, "SSH tunnel"):
            ftwctl.API("http://192.0.2.10:8080", "secret")
        ftwctl.API("http://127.0.0.1:18080", "secret")

    def test_fast_backup_watch_reports_final_state(self):
        output = io.StringIO()
        with redirect_stdout(output):
            ftwctl.watch(lambda: {"phase": "complete", "completed_bytes": 10, "total_bytes": 10},
                         lambda: False, ftwctl.time.monotonic(), "backup")
        self.assertIn("complete", output.getvalue())
        self.assertIn("100%", output.getvalue())

    def test_old_install_cannot_change_channel_or_update(self):
        api = FakeAPI({
            ("GET", "/api/version/check"): ftwctl.FTWError("HTTP 503: self-update disabled"),
            ("GET", "/api/status"): {"version": "v3.8.0-beta.1"},
        })
        with self.assertRaisesRegex(ftwctl.FTWError, "old installation"):
            ftwctl.update(api, "beta", None, 60)
        self.assertEqual(api.calls, [("GET", "/api/version/check"), ("GET", "/api/status")])

    def test_native_update_uses_core_status_until_verified_done(self):
        class UpdateAPI:
            def __init__(self):
                self.states = iter([{"state": "snapshotting", "step": 1, "total_steps": 4},
                                    {"state": "pulling", "step": 2, "total_steps": 4,
                                     "progress_current": 25, "progress_total": 100},
                                    {"state": "done", "step": 4, "total_steps": 4}])
                self.calls = []

            def json(self, method, path, body=None, timeout=15):
                self.calls.append((method, path))
                if path == "/api/version/check?force=1":
                    return {"native": True, "current": "v0.131.0-beta.1", "latest": "v0.131.1-beta.1",
                            "channel": "beta", "update_available": True, "full_backup_required": False}
                if path == "/api/version/check":
                    return {"native": True, "current": "v0.131.1-beta.1"}
                if path == "/api/version/update" and method == "POST":
                    return {"target": "v0.131.1-beta.1"}
                if path == "/api/version/update/status":
                    return next(self.states)
                if path == "/api/health":
                    return {"status": "ok"}
                raise AssertionError((method, path))

        api = UpdateAPI()
        with mock.patch.object(ftwctl.time, "sleep"), redirect_stdout(io.StringIO()):
            ftwctl.update(api, None, None, 60)
        self.assertEqual(api.calls.count(("POST", "/api/version/update")), 1)
        self.assertEqual(api.calls.count(("GET", "/api/version/update/status")), 3)

    def test_backup_copy_publishes_only_matching_verified_bytes(self):
        payload = b"full-backup-test" * 1000
        digest = hashlib.sha256(payload).hexdigest()
        entry = {"id": "ftw-full-backup-20260923T120000Z.ftwbak", "sha256": digest,
                 "size_bytes": len(payload), "verified": True}
        with tempfile.TemporaryDirectory() as directory:
            target = ftwctl.download_backup(BackupAPI(payload), entry, Path(directory), ftwctl.time.monotonic())
            self.assertEqual(target.read_bytes(), payload)
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
        with tempfile.TemporaryDirectory() as directory:
            entry["sha256"] = "0" * 64
            with self.assertRaisesRegex(ftwctl.FTWError, "does not match"):
                ftwctl.download_backup(BackupAPI(payload), entry, Path(directory), ftwctl.time.monotonic())
            self.assertEqual(list(Path(directory).iterdir()), [])

    def test_native_migration_preflight_does_not_change_service(self):
        api = FakeAPI({
            ("GET", "/api/version/check"): {"current": "v3.8.0-beta.1", "native": False},
            ("GET", "/api/health"): {"status": "ok", "drivers_ok": 3},
        })
        args = SimpleNamespace(host="homelab-rpi", tag="v0.131.0-beta.1",
            root="/opt/ftw-native", data_dir="/srv/ftw/data", config="/app/data/config.yaml",
            user_drivers="/app/data/drivers", unit="ftw.service", check_only=True,
            backup=None, max_wait=60)
        calls = []

        def fake_remote(host, *command, **kwargs):
            calls.append(command)
            if command[:2] == ("hostname", "-f"):
                return "rpi.example"
            if command[:2] == ("systemctl", "is-active"):
                return "active"
            if command[:1] == ("curl",):
                return '{"version":"v3.8.0-beta.1"}'
            if command[:2] == ("systemctl", "show"):
                return "User=ftw\nGroup=ftw\nBindPaths=/srv/ftw/data:/app/data\nExecStart={ path=/opt/ftw/ftw ; }"
            return ""

        with mock.patch.object(ftwctl, "remote", side_effect=fake_remote), mock.patch.object(ftwctl.socket, "getfqdn", return_value="mac.example"):
            ftwctl.migrate_native(api, args)
        self.assertFalse(any("stop" in call or "start" in call or "tee" in call for call in calls))

    def test_failed_native_trial_restores_old_systemd_command(self):
        with tempfile.TemporaryDirectory() as directory:
            name = "ftw-full-backup-20260923T120000Z.ftwbak"
            backup = Path(directory) / name
            backup.write_bytes(b"verified backup")
            digest = hashlib.sha256(backup.read_bytes()).hexdigest()
            api = FakeAPI({
                ("GET", "/api/version/check"): {"current": "v3.8.0-beta.1", "native": False},
                ("GET", "/api/health"): {"status": "ok", "drivers_ok": 3},
                ("GET", "/api/drivers"): {"easee": {}, "myuplink": {}, "sungrow": {}},
                ("GET", "/api/backups"): {"dir": "/app/data/backups", "backups": [{"id": name, "verified": True,
                    "sha256": digest, "size_bytes": backup.stat().st_size,
                    "created_at": datetime.now(timezone.utc).isoformat()}]},
            })
            args = SimpleNamespace(host="homelab-rpi", tag="v0.131.0-beta.1",
                root="/opt/ftw-native", data_dir="/srv/ftw/data", config="/app/data/config.yaml",
                user_drivers="/app/data/drivers", unit="ftw.service", check_only=False,
                backup=backup, max_wait=60)
            commands = []

            def fake_remote(host, *command, **kwargs):
                commands.append(command)
                if command[:2] == ("hostname", "-f"):
                    return "rpi.example"
                if command[:2] == ("systemctl", "is-active"):
                    return "active"
                if command[:1] == ("curl",):
                    return '{"version":"v3.8.0-beta.1"}'
                if command[:2] == ("systemctl", "show"):
                    return "User=ftw\nGroup=ftw\nBindPaths=/srv/ftw/data:/app/data\nExecStart={ path=/opt/ftw/ftw ; }"
                if command[:2] == ("uname", "-m"):
                    return "aarch64"
                if command[:2] == ("mktemp", "-d"):
                    return "/tmp/ftw-migrate.ABC12345"
                if command[:4] == ("sudo", "-n", "stat", "-c"):
                    return str(backup.stat().st_size)
                if "sha256sum" in command:
                    return digest + "  backup"
                if command[-1:] == ("status",):
                    return '{"current":"v0.131.0-beta.1"}'
                return ""

            def fake_package(tag, arch, work):
                files = tuple(work / name for name in ("ftw-linux-arm64.tar.gz", "ftw-linux-arm64.tar.gz.sha256", "ftw-launcher"))
                for file in files:
                    file.write_bytes(b"package")
                return files

            with mock.patch.object(ftwctl, "remote", side_effect=fake_remote), \
                 mock.patch.object(ftwctl, "remote_copy") as copied, \
                 mock.patch.object(ftwctl, "published_package", side_effect=fake_package), \
                 mock.patch.object(ftwctl, "wait_for_version", side_effect=[False, False, True]), \
                 mock.patch.object(ftwctl.socket, "getfqdn", return_value="mac.example"):
                with self.assertRaisesRegex(ftwctl.FTWError, "rolled back"):
                    ftwctl.migrate_native(api, args)
            on_box_backup = "/srv/ftw/data/backups/" + name
            self.assertIn(("sudo", "-n", "sha256sum", on_box_backup), commands)
            self.assertEqual(copied.call_count, 3)
            self.assertTrue(all(call.args[1] != backup for call in copied.call_args_list))
            self.assertIn(("sudo", "-n", "/opt/ftw-native/releases/v0.131.0-beta.1/ftw-backup",
                           "restore", "-archive", on_box_backup, "-data", "/srv/ftw/data", "-yes"), commands)
            stop = ("sudo", "-n", "systemctl", "stop", "ftw.service")
            remove = ("sudo", "-n", "rm", "-f", "/etc/systemd/system/ftw.service.d/zz-native-migration.conf")
            start = ("sudo", "-n", "systemctl", "start", "ftw.service")
            self.assertIn(remove, commands)
            self.assertEqual(commands.count(stop), 3)
            self.assertEqual(commands.count(start), 3)
            self.assertLess(commands.index(stop), commands.index(remove))


if __name__ == "__main__":
    unittest.main()
