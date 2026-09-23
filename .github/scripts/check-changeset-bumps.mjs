import { existsSync, readFileSync } from "node:fs";
import { basename } from "node:path";
import { parseChangesetFile } from "@changesets/parse";

let rejected = false;
for (const file of process.argv.slice(2)) {
  if (basename(file) === "README.md" || !existsSync(file)) continue;
  const { releases } = parseChangesetFile(readFileSync(file, "utf8"));
  if (releases.some(({ name, type }) => name === "ftw" && type === "major")) {
    console.error(`Core major bump is not allowed: ${file}`);
    rejected = true;
  }
}
if (rejected) process.exitCode = 1;
