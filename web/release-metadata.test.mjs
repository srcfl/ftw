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
const betaWorkflow = readFileSync(
  join(repoRoot, ".github", "workflows", "beta.yml"),
  "utf8",
);
const releaseAssetsWorkflow = readFileSync(
  join(repoRoot, ".github", "workflows", "release-assets.yml"),
  "utf8",
);
const localReleaseScript = readFileSync(
  join(repoRoot, "scripts", "release.sh"),
  "utf8",
);
const changesetCheckWorkflow = readFileSync(
  join(repoRoot, ".github", "workflows", "changeset-check.yml"),
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

  it("validates Changesets against the fetched PR base", () => {
    assert.match(
      changesetCheckWorkflow,
      /changeset status --since=["']origin\/\$\{\{ github\.base_ref \}\}["']/,
    );
  });

  it("keeps stable draft-only until the immutable control-plane pair passes", () => {
    assert.match(releaseWorkflow, /gh release create "\$\{TAG\}"[\s\S]+--draft/);
    assert.match(releaseWorkflow, /verify-control-plane-manifest\.sh[\s\S]+"\$\{BETA_TAG\}"/);
    assert.match(releaseAssetsWorkflow, /name: verify pair and publish release/);
    assert.match(releaseAssetsWorkflow, /gh release edit "\$\{TAG\}"[\s\S]+--draft=false/);
    assert.match(releaseAssetsWorkflow, /needs: \[meta, control-plane\]/);
  });

  it("requires the same-release Core and updater digests on beta and stable", () => {
    for (const workflow of [betaWorkflow, releaseAssetsWorkflow]) {
      assert.match(workflow, /create-control-plane-manifest\.sh/);
      assert.match(workflow, /verify-control-plane-manifest\.sh/);
      assert.match(workflow, /ftw-control-plane\.json/);
      assert.match(workflow, /Refuse to move an existing immutable tag/);
    }
  });

  it("does not let the local release helper create or publish releases", () => {
    assert.doesNotMatch(localReleaseScript, /gh release create/);
    assert.doesNotMatch(localReleaseScript, /gh release edit/);
    assert.match(localReleaseScript, /isDraft/);
    assert.match(localReleaseScript, /ftw-control-plane\.json/);
  });
});
