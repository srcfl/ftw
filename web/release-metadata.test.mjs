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

  it("resolves no-changeset before fail-closed Changesets validation", () => {
    const labelDecision = changesetCheckWorkflow.indexOf(
      "- name: Short-circuit on no-changeset label",
    );
    const setup = changesetCheckWorkflow.indexOf(
      "- name: Setup Node for Changesets validation",
    );
    const install = changesetCheckWorkflow.indexOf(
      "- name: Install Changesets dependencies",
    );
    const validate = changesetCheckWorkflow.indexOf(
      "- name: Validate pending Changesets",
    );
    const guard =
      "if: steps.version_pr.outputs.skip != 'true' && steps.label.outputs.skip != 'true'";

    assert.ok(labelDecision >= 0, "workflow must resolve the label exemption");
    assert.ok(labelDecision < setup && setup < install && install < validate);
    for (const step of [setup, install, validate]) {
      assert.equal(
        changesetCheckWorkflow.slice(step, step + 240).includes(guard),
        true,
        "every Changesets validation step must use the label guard",
      );
    }
    assert.match(changesetCheckWorkflow, /GITHUB_STEP_SUMMARY/);
    assert.match(changesetCheckWorkflow, /npx changeset status/);
    assert.doesNotMatch(changesetCheckWorkflow, /continue-on-error/);
  });
});
