import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const here = dirname(fileURLToPath(import.meta.url));
const script = join(here, "check-changeset-bumps.mjs");

test("rejects every valid major frontmatter form and accepts patch or minor", () => {
  const dir = mkdtempSync(join(tmpdir(), "ftw-changeset-bumps-"));
  try {
    const cases = [
      ["patch", '---\n"ftw": patch\n---\ntext\n', 0],
      ["minor", "---\nftw: minor\n---\ntext\n", 0],
      ["major", '---\n"ftw": major\n---\ntext\n', 1],
      ["flow map", '---\n{"ftw": "major"}\n---\ntext\n', 1],
      ["YAML anchor", "---\nftw: &release major\n---\ntext\n", 1],
      ["body text", '---\n"ftw": patch\n---\nA major change.\n', 0],
    ];
    for (const [name, body, want] of cases) {
      const file = join(dir, "example.md");
      writeFileSync(file, body);
      const result = spawnSync(process.execPath, [script, file], { encoding: "utf8" });
      assert.equal(result.status, want, `${name}: ${result.stderr}`);
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("CI checks renamed changesets", () => {
  const workflow = readFileSync(join(here, "..", "workflows", "changeset-check.yml"), "utf8");
  assert.match(workflow, /git diff -M --name-only --diff-filter=ACMR/);
});
