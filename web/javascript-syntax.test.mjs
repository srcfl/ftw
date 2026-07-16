import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));

function firstPartyJavaScript(dir) {
  const files = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === "vendor") continue;
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...firstPartyJavaScript(path));
    } else if (entry.isFile() && entry.name.endsWith(".js")) {
      files.push(path);
    }
  }
  return files.sort();
}

describe("first-party browser JavaScript", () => {
  it("is syntactically valid", () => {
    for (const path of firstPartyJavaScript(webRoot)) {
      const result = spawnSync(process.execPath, ["--check", path], {
        encoding: "utf8",
      });
      assert.equal(
        result.status,
        0,
        `${relative(webRoot, path)} failed syntax validation:\n${result.stderr || result.stdout}`,
      );
    }
  });

  it("uses one module URL per shared component", () => {
    const variantsByModule = new Map();
    const componentsRoot = join(webRoot, "components");
    for (const path of firstPartyJavaScript(componentsRoot)) {
      const source = readFileSync(path, "utf8");
      for (const match of source.matchAll(/["'](\.\/[^"']+\.js(?:\?[^"']*)?)["']/g)) {
        const specifier = match[1];
        const canonical = specifier.split("?", 1)[0];
        const variants = variantsByModule.get(canonical) || new Set();
        variants.add(specifier);
        variantsByModule.set(canonical, variants);
      }
    }

    for (const [canonical, variants] of variantsByModule) {
      assert.equal(
        variants.size,
        1,
        `${canonical} is imported through multiple URLs and would execute more than once: ${[...variants].join(", ")}`,
      );
    }
  });
});
