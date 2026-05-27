// semantic-release plugin — prepends a Hitchhiker-themed header to the
// notes produced by @semantic-release/release-notes-generator.
//
// Cosmetic only. Semver numbers, tags, pre-release channels, and Docker
// tags are untouched so every downstream tool (deploy scripts, image
// sort, dependency comparisons) keeps working.
//
// Rules:
// - Every release gets a codename. Deterministic per minor, so patches
//   under a minor share the same name. The rotation cycles once the
//   list is exhausted — that's the trade for not curating a one-shot
//   list per release.
// - "Don't Panic." sits below the codename on every release.
// - When any of MAJOR / MINOR / PATCH equals 42, an extra line names
//   the ceremony.

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
  // Small per-major offset so the same minor across major bumps gets a
  // visibly different name once we ever bump major.
  const idx = (major * 7 + minor) % NAMES.length;
  return NAMES[idx];
}

function hasFortyTwo(major, minor, patch) {
  // Trigger when "42" appears anywhere in MAJOR.MINOR.PATCH after
  // stripping the dots — catches 0.42.x, x.0.42, 4.2.0, 1.4.2, …
  return `${major}${minor}${patch}`.includes('42');
}

async function generateNotes(_pluginConfig, context) {
  const next = (context && context.nextRelease) || {};
  const m = /^(\d+)\.(\d+)\.(\d+)/.exec(next.version || '');
  if (!m) return next.notes || '';
  const major = parseInt(m[1], 10);
  const minor = parseInt(m[2], 10);
  const patch = parseInt(m[3], 10);

  const codename = pickCodename(major, minor);
  const ceremony = hasFortyTwo(major, minor, patch)
    ? "\n> ✨ **The Answer arrives.** This release carries the number 42. Don't forget your towel.\n"
    : '';

  const header =
    `## 🛰️ ${codename}\n\n` +
    `> _Don't Panic._\n` +
    ceremony +
    `\n`;

  return header + (next.notes || '');
}

module.exports = { generateNotes };
