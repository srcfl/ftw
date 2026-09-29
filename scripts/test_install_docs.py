"""Run the copyable install blocks with fake downloads and Docker calls."""
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
EN = ROOT / 'docs/native-beta.md'
SV = ROOT / 'docs/setup-guide/update-sv.md'


def recipe(path, marker):
    blocks = re.findall(r'```bash\n(.*?)\n```', path.read_text(), re.S)
    matches = [block for block in blocks if marker in block and 'tag=v0.X.Y-beta.N' in block]
    assert len(matches) == 1, (path, marker, len(matches))
    return matches[0]


class InstallDocsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='ftw-install-docs-')
        self.addCleanup(self.tmp.cleanup)
        self.work = Path(self.tmp.name)
        self.bin = self.work / 'bin'
        self.bin.mkdir()
        self.log = self.work / 'calls'
        self.project = self.work / 'ftw-local'
        self.env = dict(os.environ, PATH=f'{self.bin}:{os.environ["PATH"]}',
                        FTW_TEST_LOG=str(self.log), FTW_TEST_FAIL='')
        self.stub('sudo', 'exec "$@"')
        self.stub('chown', 'echo "chown $*" >> "$FTW_TEST_LOG"')
        self.stub('docker', '''echo "docker $*" >> "$FTW_TEST_LOG"
if [ "$FTW_TEST_FAIL" = buildx ] && [ "$1" = buildx ]; then exit 1; fi''')
        self.stub('curl', '''echo "curl $*" >> "$FTW_TEST_LOG"
case "$*" in
  *Dockerfile*) [ "$FTW_TEST_FAIL" != dockerfile ] || exit 22 ;;
esac
[ "$FTW_TEST_FAIL" != download ] || exit 22
while [ "$1" != -o ]; do shift; done
shift
printf '%s\\n' 'echo INSTALL >> "$FTW_TEST_LOG"' > "$1"''')

    def stub(self, name, body):
        path = self.bin / name
        path.write_text('#!/usr/bin/env bash\nset -eu\n' + body + '\n')
        path.chmod(0o755)

    def run_recipe(self, marker, failure='', tag='v0.138.2-beta.1'):
        block = recipe(EN, marker).replace('v0.X.Y-beta.N', tag)
        # Change only the destination, never the user's HOME or real Docker state.
        if marker == 'compose up':
            self.assertEqual(block.count('"$HOME/ftw-local"'), 2)
            block = block.replace('"$HOME/ftw-local"', shlex.quote(str(self.project)))
        self.env['FTW_TEST_FAIL'] = failure
        result = subprocess.run(['bash', '-c', block], cwd=self.work, env=self.env,
                                capture_output=True, text=True)
        calls = self.log.read_text() if self.log.exists() else ''
        return result, calls

    def test_translations_use_the_same_native_commands(self):
        self.assertEqual(recipe(EN, '--fresh-host'), recipe(SV, '--fresh-host'))

    def test_rejects_placeholder_old_line_and_mistyped_tag_before_work(self):
        for marker in ('--fresh-host', 'compose up'):
            for tag in ('v0.X.Y-beta.N', 'v3.8.0-beta.1', 'v0.130.0', 'v0.0.138-beta.1'):
                with self.subTest(marker=marker, tag=tag):
                    result, calls = self.run_recipe(marker, tag=tag)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(calls, '')
                    self.assertFalse(self.project.exists())

    def test_native_download_failure_does_not_run_stale_installer(self):
        (self.work / 'install.sh').write_text('echo STALE >> "$FTW_TEST_LOG"\n')
        result, calls = self.run_recipe('--fresh-host', 'download')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('INSTALL', calls)
        self.assertNotIn('STALE', calls)
        download = shlex.split(calls.strip())[-1]
        self.assertFalse(Path(download).exists())

    def test_native_success_runs_download_and_removes_temp_file(self):
        result, calls = self.run_recipe('--fresh-host')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('INSTALL\n', calls)
        download = shlex.split(calls.splitlines()[0])[-1]
        self.assertFalse(Path(download).exists())

    def test_second_docker_download_failure_stops_before_data_env_or_build(self):
        result, calls = self.run_recipe('compose up', 'dockerfile')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.project / '.env').exists())
        self.assertFalse((self.project / 'data').exists())
        self.assertNotIn('chown', calls)
        self.assertNotIn('docker compose up', calls)

    def test_existing_project_stays_untouched(self):
        self.project.mkdir()
        (self.project / '.env').write_text('keep previous version\n')
        result, calls = self.run_recipe('compose up')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.project / '.env').read_text(), 'keep previous version\n')
        self.assertNotIn('curl', calls)
        self.assertNotIn('docker compose up', calls)

    def test_missing_buildx_stops_before_creating_project(self):
        result, calls = self.run_recipe('compose up', 'buildx')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.project.exists())
        self.assertNotIn('curl', calls)

    def test_docker_success_writes_ignore_before_build(self):
        # Make the fake builder inspect the inputs at the instant of the build.
        self.stub('docker', '''echo "docker $*" >> "$FTW_TEST_LOG"
if [ "$1 ${2:-}" = 'compose up' ]; then
  cmp .dockerignore "$FTW_TEST_IGNORE"
  test -d data
  test -f compose.yaml
  test -f Dockerfile
  grep -qx 'FTW_VERSION=v0.138.2-beta.1' .env
fi''')
        self.env['FTW_TEST_IGNORE'] = str(ROOT / 'deploy/docker/.dockerignore')
        result, calls = self.run_recipe('compose up')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('docker compose up -d --build', calls)


if __name__ == '__main__':
    unittest.main()
