"""Check the install line and run the copyable Docker blocks with fakes."""
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
EN = ROOT / 'docs/native-beta.md'

# The one line every page gives for a first install on Linux. ftw-web serves
# /install.sh as a redirect to scripts/install.sh on master.
INSTALL_LINE = 'curl -fsSL https://ftw.energy/install.sh | bash -s -- --fresh-host'
INSTALL_LINE_PAGES = (
    'README.md',
    'docs/install.md',
    'docs/native-beta.md',
    'docs/setup-guide/install-sv.md',
    'docs/setup-guide/update-sv.md',
    'docs/setup-guide/en.md',
    'docs/setup-guide/sv.md',
    'docs/setup-guide/de.md',
    'docs/setup-guide/fr.md',
    'docs/setup-guide/es.md',
)

# A fenced block, also one indented under a list item.
FENCE = re.compile(r'^([ \t]*)```[^\n]*\n(.*?)\n\1```[ \t]*$', re.M | re.S)


def code_blocks(path):
    """Every fenced block in a page, without the list indent."""
    blocks = []
    for indent, body in FENCE.findall(path.read_text()):
        lines = body.split('\n')
        blocks.append('\n'.join(line[len(indent):] if line.startswith(indent) else line
                                for line in lines))
    return blocks


def install_blocks(path):
    return [block for block in code_blocks(path) if '--fresh-host' in block]


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
            self.assertEqual(block.count('"$HOME/ftw-local"'), 1)
            block = block.replace('"$HOME/ftw-local"', shlex.quote(str(self.project)))
        self.env['FTW_TEST_FAIL'] = failure
        result = subprocess.run(['bash', '-c', block], cwd=self.work, env=self.env,
                                capture_output=True, text=True)
        calls = self.log.read_text() if self.log.exists() else ''
        return result, calls

    def test_each_install_page_shows_the_install_line_once(self):
        for name in INSTALL_LINE_PAGES:
            with self.subTest(page=name):
                self.assertEqual(install_blocks(ROOT / name), [INSTALL_LINE])

    def test_no_other_page_shows_an_install_block(self):
        # A new page with the line joins the list; a page never carries
        # another way to run --fresh-host.
        pages = [ROOT / 'README.md', *sorted((ROOT / 'docs').rglob('*.md'))]
        for page in pages:
            blocks = install_blocks(page)
            if not blocks:
                continue
            with self.subTest(page=str(page.relative_to(ROOT))):
                self.assertIn(str(page.relative_to(ROOT)), INSTALL_LINE_PAGES)
                self.assertEqual(blocks, [INSTALL_LINE])

    def run_line(self, curl_body):
        self.stub('curl', 'echo "curl $*" >> "$FTW_TEST_LOG"\n' + curl_body)
        result = subprocess.run(['bash', '-c', INSTALL_LINE], cwd=self.work, env=self.env,
                                capture_output=True, text=True)
        calls = self.log.read_text() if self.log.exists() else ''
        return result, calls

    def test_install_line_runs_the_fetched_installer_with_fresh_host(self):
        result, calls = self.run_line(
            """printf '%s\\n' 'echo "INSTALL $*" >> "$FTW_TEST_LOG"'""")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, 'curl -fsSL https://ftw.energy/install.sh\n'
                                'INSTALL --fresh-host\n')

    def test_install_line_runs_nothing_after_a_failed_download(self):
        # curl -f prints no error page, so bash gets nothing to run.
        _, calls = self.run_line('exit 22')
        self.assertEqual(calls, 'curl -fsSL https://ftw.energy/install.sh\n')

    def test_rejects_placeholder_old_line_and_mistyped_tag_before_work(self):
        for tag in ('v0.X.Y-beta.N', 'v3.8.0-beta.1', 'v0.130.0', 'v0.0.138-beta.1'):
            with self.subTest(tag=tag):
                result, calls = self.run_recipe('compose up', tag=tag)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, '')
                self.assertFalse(self.project.exists())

    def test_failed_docker_download_leaves_no_project_and_allows_retry(self):
        for failure in ('download', 'dockerfile'):
            with self.subTest(failure=failure):
                result, calls = self.run_recipe('compose up', failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.project.exists())
                self.assertNotIn('chown', calls)
                self.assertNotIn('docker compose up', calls)
                for call in calls.splitlines():
                    if call.startswith('curl '):
                        self.assertFalse(Path(shlex.split(call)[-1]).parent.exists())
        result, calls = self.run_recipe('compose up')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('docker compose up -d --build', calls)

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

    def run_move(self, failure='', tag='v0.170.0-beta.1'):
        """Run the block that moves an existing Docker project to the launcher."""
        block = recipe(EN, "grep -v '^FTW_VERSION='").replace('v0.X.Y-beta.N', tag)
        self.env['FTW_TEST_FAIL'] = failure
        result = subprocess.run(['bash', '-c', block], cwd=self.project, env=self.env,
                                capture_output=True, text=True)
        calls = self.log.read_text() if self.log.exists() else ''
        return result, calls

    def old_project(self):
        (self.project / 'data').mkdir(parents=True)
        (self.project / 'data/state.db').write_text('household record\n')
        (self.project / 'compose.yaml').write_text('old compose\n')
        (self.project / 'Dockerfile').write_text('old Dockerfile\n')
        (self.project / '.env').write_text('COMPOSE_PROFILES=x\nFTW_VERSION=v0.150.0-beta.1\n')

    def test_docker_move_replaces_files_and_version_only(self):
        self.old_project()
        result, calls = self.run_move()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('/v0.170.0-beta.1/deploy/docker/compose.yaml', calls)
        for name in ('compose.yaml', 'Dockerfile'):
            self.assertIn('INSTALL', (self.project / name).read_text())
        self.assertEqual((self.project / '.env').read_text(),
                         'COMPOSE_PROFILES=x\nFTW_VERSION=v0.170.0-beta.1\n')
        self.assertEqual((self.project / 'data/state.db').read_text(), 'household record\n')
        # It only prepares the files; the owner rebuilds as the next step.
        self.assertFalse([c for c in calls.splitlines() if c.startswith('docker ')])
        for call in calls.splitlines():
            self.assertFalse(Path(shlex.split(call)[-1]).parent.exists())

    def test_docker_move_failure_leaves_the_project(self):
        for failure in ('download', 'dockerfile'):
            with self.subTest(failure=failure):
                if self.project.exists():
                    for path in sorted(self.project.rglob('*'), reverse=True):
                        path.unlink() if path.is_file() else path.rmdir()
                self.old_project()
                result, _ = self.run_move(failure)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual((self.project / 'compose.yaml').read_text(), 'old compose\n')
                self.assertEqual((self.project / 'Dockerfile').read_text(), 'old Dockerfile\n')
                self.assertIn('FTW_VERSION=v0.150.0-beta.1', (self.project / '.env').read_text())

    def test_docker_move_refuses_a_bad_tag_or_another_directory(self):
        self.old_project()
        result, calls = self.run_move(tag='v0.X.Y-beta.N')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, '')
        (self.project / '.env').unlink()
        result, calls = self.run_move()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('STOP: run this in the Compose project', result.stderr)
        self.assertEqual(calls, '')


if __name__ == '__main__':
    unittest.main()
