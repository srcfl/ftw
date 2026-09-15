import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
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
const stateSchema = JSON.parse(
  readFileSync(join(repoRoot, "state-schema.json"), "utf8"),
);
const changesetCheckWorkflow = readFileSync(
  join(repoRoot, ".github", "workflows", "changeset-check.yml"),
  "utf8",
);

function workflowRun(stepName) {
  const stepMarker = `      - name: ${stepName}\n`;
  const stepStart = changesetCheckWorkflow.indexOf(stepMarker);
  assert.ok(stepStart >= 0, `workflow step not found: ${stepName}`);

  const nextStep = changesetCheckWorkflow.indexOf("\n      - name:", stepStart + 1);
  const step = changesetCheckWorkflow.slice(
    stepStart,
    nextStep >= 0 ? nextStep : undefined,
  );
  const runMarker = "        run: |\n";
  const runStart = step.indexOf(runMarker);
  assert.ok(runStart >= 0, `workflow run block not found: ${stepName}`);

  return step
    .slice(runStart + runMarker.length)
    .split("\n")
    .map((line) => (line.startsWith("          ") ? line.slice(10) : line))
    .join("\n");
}

function runWorkflowStep(stepName, env) {
  const directory = mkdtempSync(join(tmpdir(), "ftw-changeset-policy-"));
  const outputPath = join(directory, "output");
  const summaryPath = join(directory, "summary");
  let result;
  let output;
  let summary;

  try {
    result = spawnSync("bash", ["-c", workflowRun(stepName)], {
      encoding: "utf8",
      env: {
        ...process.env,
        ...env,
        GITHUB_OUTPUT: outputPath,
        GITHUB_STEP_SUMMARY: summaryPath,
      },
    });
    output = existsSync(outputPath) ? readFileSync(outputPath, "utf8") : "";
    summary = existsSync(summaryPath) ? readFileSync(summaryPath, "utf8") : "";
  } finally {
    rmSync(directory, { force: true, recursive: true });
  }

  assert.equal(result.status, 0, result.stderr || result.stdout);
  return { output, summary };
}

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
    assert.match(releaseWorkflow, /version-script:\s+npm run version-packages/);
  });

  it("reads the changesets output name its action major publishes", () => {
    // changesets/action v2 renamed every input and output to kebab-case.
    // A stale `hasChangesets` reads as empty, so the stable promotion
    // steps guarded by it would skip in silence and release nothing.
    assert.match(releaseWorkflow, /uses: changesets\/action@v2\b/);
    assert.doesNotMatch(releaseWorkflow, /outputs\.hasChangesets/);
    assert.match(releaseWorkflow, /outputs\.has-changesets/);
  });

  it("publishes the state schema in beta and stable release notes", () => {
    assert.ok(Number.isInteger(stateSchema.version) && stateSchema.version > 0);
    assert.match(betaWorkflow, /require\('\.\/state-schema\.json'\)\.version/);
    assert.match(betaWorkflow, /--notes "<!-- ftw-state-schema:\$\{STATE_SCHEMA\} -->"/);
    assert.match(releaseWorkflow, /require\('\.\/state-schema\.json'\)\.version/);
    assert.match(
      releaseWorkflow,
      /<!-- ftw-state-schema:%s -->\\n' "\$\{STATE_SCHEMA\}" >> release-notes\.md/,
    );
  });

  it("validates Changesets against the fetched PR base", () => {
    assert.match(
      changesetCheckWorkflow,
      /changeset status --since=["']origin\/\$\{\{ github\.base_ref \}\}["']/,
    );
  });

  it("only exempts the exact trusted Version PR identity", () => {
    const canonical = {
      TITLE: "chore(release): version packages",
      HEAD_REPO: "srcfl/ftw",
      HEAD_REF: "changeset-release/master",
      BASE_REF: "master",
      GITHUB_REPOSITORY: "srcfl/ftw",
    };
    const cases = [
      ["canonical Version PR", canonical, "skip=true\n"],
      ["ordinary head", { ...canonical, HEAD_REF: "agent/title-spoof" }, "skip=false\n"],
      ["fork head", { ...canonical, HEAD_REPO: "attacker/ftw" }, "skip=false\n"],
      ["wrong base", { ...canonical, BASE_REF: "release" }, "skip=false\n"],
      [
        "title prefix",
        { ...canonical, TITLE: "chore(release): version packages spoof" },
        "skip=false\n",
      ],
    ];

    for (const [name, env, expected] of cases) {
      const { output } = runWorkflowStep(
        "Short-circuit on auto-generated Version PR",
        env,
      );
      assert.equal(output, expected, name);
    }
  });

  it("only exempts an explicit no-changeset label", () => {
    const labeled = runWorkflowStep("Short-circuit on no-changeset label", {
      LABELS: JSON.stringify([{ name: "no-changeset" }]),
    });
    const unlabeled = runWorkflowStep("Short-circuit on no-changeset label", {
      LABELS: "[]",
    });

    assert.equal(labeled.output, "skip=true\n");
    assert.match(labeled.summary, /no-changeset.*skips Changesets/);
    assert.equal(unlabeled.output, "skip=false\n");
    assert.equal(unlabeled.summary, "");
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
    const presence = changesetCheckWorkflow.indexOf(
      "- name: Verify changeset present (or PR is allowlisted)",
    );
    // Checked as two separate conditions rather than one exact string: a
    // step may carry a further guard of its own — the allowlist verdict
    // does — and matching the concatenation would read that as a removed
    // short-circuit.
    const guards = [
      "steps.version_pr.outputs.skip != 'true'",
      "steps.label.outputs.skip != 'true'",
    ];

    assert.ok(labelDecision >= 0, "workflow must resolve the label exemption");
    assert.ok(labelDecision < setup && setup < install && install < validate);
    // `changeset status` fails whenever a package changed and no changeset
    // was added, which is true of a docs-only PR. Reaching it before the
    // allowlist verdict failed exactly the PRs the allowlist exempts.
    assert.ok(
      presence >= 0 && presence < setup,
      "the allowlist verdict must be resolved before Changesets validation runs",
    );
    for (const step of [setup, install, validate, presence]) {
      for (const guard of guards) {
        assert.equal(
          changesetCheckWorkflow.slice(step, step + 320).includes(guard),
          true,
          `every Changesets validation step must keep the guard: ${guard}`,
        );
      }
    }
    assert.match(changesetCheckWorkflow, /GITHUB_STEP_SUMMARY/);
    assert.match(changesetCheckWorkflow, /npx changeset status/);
    assert.doesNotMatch(changesetCheckWorkflow, /continue-on-error/);
  });

  it("checks the completed paired beta before any stable write", () => {
    const prereleaseCheck = releaseWorkflow.indexOf(
      'BETA_RELEASE_JSON="$(gh release view "${BETA_TAG}"',
    );
    const coreImageCheck = releaseWorkflow.indexOf(
      'verify_beta_image "ghcr.io/srcfl/ftw:${BETA_TAG}"',
    );
    const updaterImageCheck = releaseWorkflow.indexOf(
      'verify_beta_image "ghcr.io/srcfl/ftw-updater:${BETA_TAG}"',
    );
    const stableTagWrite = releaseWorkflow.indexOf('git tag -a "${TAG}"');
    const stableReleaseWrite = releaseWorkflow.indexOf(
      'gh release create "${TAG}"',
    );
    const assetDispatch = releaseWorkflow.lastIndexOf(
      "gh workflow run release-assets.yml",
    );

    assert.ok(prereleaseCheck >= 0, "stable must require a completed beta release");
    assert.ok(coreImageCheck >= 0, "stable must inspect the Core beta image");
    assert.ok(updaterImageCheck >= 0, "stable must inspect the updater beta image");
    assert.ok(stableTagWrite > updaterImageCheck, "beta checks must precede the stable tag");
    assert.ok(
      stableReleaseWrite > updaterImageCheck,
      "beta checks must precede the stable release",
    );
    assert.ok(assetDispatch > updaterImageCheck, "beta checks must precede asset dispatch");
    assert.match(releaseWorkflow, /BETA_COMMIT.+GITHUB_SHA/s);
    assert.match(releaseWorkflow, /\.tagName == \$tag/);
    assert.match(releaseWorkflow, /\.isDraft == false/);
    assert.match(releaseWorkflow, /\.isPrerelease == true/);
    assert.match(releaseWorkflow, /\.publishedAt/);
    assert.match(
      releaseWorkflow,
      /python3 - "\$\{metadata\}" "\$\{GITHUB_SHA\}" "\$\{VERSION\}"/,
    );
    assert.match(releaseWorkflow, /current_digest.+expected_digest/s);
    assert.match(releaseWorkflow, /image_ref.+expected_digest/s);
    assert.match(releaseWorkflow, /required_platforms = \("linux\/amd64", "linux\/arm64"\)/);
    assert.match(releaseWorkflow, /org\.opencontainers\.image\.revision/);
    assert.match(releaseWorkflow, /org\.opencontainers\.image\.version/);
  });
});
