import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const packageJSON = JSON.parse(readFileSync(join(repoRoot, "package.json"), "utf8"));
const packageLock = JSON.parse(readFileSync(join(repoRoot, "package-lock.json"), "utf8"));
const lockRoot = packageLock.packages?.[""];
const releaseWorkflow = readFileSync(
  join(repoRoot, ".github", "workflows", "release.yml"),
  "utf8",
);

describe("release metadata", () => {
  it("keeps package.json and package-lock.json root identity in sync", () => {
    assert.ok(lockRoot, "package-lock.json must describe the root package");
    assert.equal(packageLock.name, packageJSON.name);
    assert.equal(packageLock.version, packageJSON.version);
    assert.equal(lockRoot.name, packageJSON.name);
    assert.equal(lockRoot.version, packageJSON.version);
  });

  it("keeps the generated Version Packages PR lockfile-aware", () => {
    assert.match(
      packageJSON.scripts?.["version-packages"] || "",
      /changeset version.+npm install --package-lock-only/,
    );
    assert.match(releaseWorkflow, /version:\s+npm run version-packages/);
  });
});
