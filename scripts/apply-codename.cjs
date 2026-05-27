#!/usr/bin/env node
// Prepend a Hitchhiker-themed header to the latest CHANGELOG.md section
// AND emit the same notes to stdout for `gh release create --notes-file`.
//
// Invoked by the release workflow after `changeset version` has updated
// CHANGELOG.md. Cosmetic only — semver numbers, tags, and Docker tags
// are untouched.
//
// Usage:
//   node scripts/apply-codename.cjs <version> > release-notes.md
//
// Where <version> is the bumped version from package.json (no leading v).

const fs = require('fs');
const path = require('path');

const NAMES = [
  'Heart of Gold',
  'Magrathea',
  'Slartibartfast',
  'Pan Galactic',
  'Vogon Constructor Fleet',
  'Babel Fish',
  'Deep Thought',
  'Hyperspace Bypass',
  'Mostly Harmless',
  'Improbability Drive',
  'Marvin',
  'Trillian',
  'Zaphod Beeblebrox',
  'Arthur Dent',
  'Ford Prefect',
  'Sub-Etha Sens-O-Matic',
  'Bistromathic Drive',
  'Pan Galactic Gargle Blaster',
  'Plural Z Alpha',
  'Eddie the Shipboard Computer',
  'Sperm Whale',
  'Bowl of Petunias',
  'Frogstar World B',
  'Lamuella',
  'Krikkit',
  'Stavromula Beta',
  'Disaster Area',
  'Sirius Cybernetics',
  'Megadodo Publications',
  'Maximegalon',
  'Wonko the Sane',
  'Damogran',
  'Eccentrica Gallumbits',
  'Long Dark Tea-Time',
  'Total Perspective Vortex',
  'Restaurant at the End of the Universe',
  'Towel Day',
  'Vogon Poetry',
  'Infinitely Improbable',
  'Brockian Ultra-Cricket',
];

function pickCodename(major, minor) {
  const idx = (major * 7 + minor) % NAMES.length;
  return NAMES[idx];
}

function hasFortyTwo(major, minor, patch) {
  return `${major}${minor}${patch}`.includes('42');
}

function extractLatestSection(changelogPath, version) {
  // CHANGELOG.md from @changesets/cli uses `## <version>` for each
  // release. Pull everything from the first `## <version>` line up to
  // (but not including) the next `## ` heading.
  const raw = fs.readFileSync(changelogPath, 'utf8');
  const lines = raw.split('\n');
  const header = `## ${version}`;
  const startIdx = lines.findIndex((l) => l.trim() === header);
  if (startIdx === -1) {
    throw new Error(`No "${header}" section found in ${changelogPath}`);
  }
  let endIdx = lines.length;
  for (let i = startIdx + 1; i < lines.length; i++) {
    if (lines[i].startsWith('## ')) {
      endIdx = i;
      break;
    }
  }
  // Skip the heading itself — we replace it with the codename header
  // when writing the release notes; the CHANGELOG keeps the heading.
  return lines.slice(startIdx + 1, endIdx).join('\n').trim();
}

function buildHeader(version) {
  const m = /^(\d+)\.(\d+)\.(\d+)/.exec(version);
  if (!m) throw new Error(`Invalid version: ${version}`);
  const major = parseInt(m[1], 10);
  const minor = parseInt(m[2], 10);
  const patch = parseInt(m[3], 10);

  const codename = pickCodename(major, minor);
  const ceremony = hasFortyTwo(major, minor, patch)
    ? "\n> ✨ **The Answer arrives.** This release carries the number 42. Don't forget your towel.\n"
    : '';

  return (
    `## 🛰️ ${codename}\n\n` +
    `> _Don't Panic._\n` +
    ceremony +
    `\n`
  );
}

function main() {
  const version = process.argv[2];
  if (!version) {
    console.error('usage: apply-codename.cjs <version>');
    process.exit(2);
  }
  const changelogPath = path.resolve(__dirname, '..', 'CHANGELOG.md');
  const body = extractLatestSection(changelogPath, version);
  const header = buildHeader(version);
  process.stdout.write(header + body + '\n');
}

main();
