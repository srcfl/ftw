import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const badge = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");

test("update dialog exposes stable and beta as a segmented channel control", () => {
  assert.match(badge, /\["stable", "beta"\]/);
  assert.match(badge, /role="group" aria-label="Update channel"/);
  assert.match(badge, /aria-pressed=/);
  assert.match(badge, /grid-auto-columns: minmax\(0, 1fr\)/);
  assert.doesNotMatch(badge, /grid-template-columns: repeat\(3,/);
});

test("both channel controls sit together so a split channel is visible", () => {
  assert.match(badge, /_channelSectionHTML\(\)/);
  assert.match(badge, /role="group" aria-label="Optimizer update channel"/);
  assert.match(badge, /Optimizer tracks \$\{escapeHTML\(optimizerChannel\)\} while Core tracks/);
  // The optimizer channel used to be a second, unlabelled control buried in
  // the component row with no stated relation to the global one.
  assert.doesNotMatch(badge, /mini-channel/);
});

test("the channel control does not claim to govern drivers", () => {
  // selfupdate.Channel drives the Core image only; driverrepo never reads it.
  // A driver is pinned to an exact version and takes its channel per install
  // (?channel=beta on the catalog, {version, channel} on the install call),
  // so a host is never "on" a driver channel.
  assert.match(badge, /Drivers follow no channel\./);
  assert.match(badge, /pinned to a version you pick per driver/);
  assert.doesNotMatch(badge, /Core &amp; drivers/);
  assert.doesNotMatch(badge, /Core and drivers track/);
});

test("the summary counts the whole inventory and says when it last checked", () => {
  assert.match(badge, /Everything is up to date\./);
  assert.match(badge, /\$\{pending\.total\} updates available\./);
  assert.match(badge, /info\.checked_at \? Date\.parse\(info\.checked_at\) : 0/);
  assert.match(badge, /Checked \$\{new Date\(checked\)\.toLocaleTimeString/);
  assert.match(badge, /Not checked yet\./);
  // Core's version was printed three times: twice here and once in the
  // component card. The card is now the only place it appears.
  assert.doesNotMatch(badge, /<dt>Core current<\/dt>/);
  assert.doesNotMatch(badge, /<dt>Latest<\/dt>/);
});

test("every component keeps a row whether or not it has an update", () => {
  assert.match(badge, /<table class="inventory-table">/);
  assert.match(badge, /<th scope="col">Component<\/th><th scope="col">Version<\/th><th scope="col">Status<\/th>/);
  assert.match(badge, /const coreStatus = info\.update_available/);
  assert.match(badge, /const optimizerStatus = !optimizer\.configured/);
  // Internal wording that told the operator nothing they could act on.
  assert.doesNotMatch(badge, /safety authority · updated with updater/);
});

test("the inventory is one table so its columns line up across rows", () => {
  // Per-row grids sized themselves independently, so Core's version sat in a
  // different place from a driver's.
  assert.doesNotMatch(badge, /class="component-row"/);
  assert.match(badge, /\.inventory-table \{[\s\S]*?border-collapse: collapse;/);
  assert.match(badge, /driver-history-row"><td colspan="4"/);
  // Stacked mobile rows must not inherit the nowrap that sizes the desktop
  // action column, or the dialog scrolls sideways on a phone.
  assert.match(badge, /@media \(max-width: 560px\)[\s\S]*?white-space: normal;/);
});

test("the optimizer row only draws an arrow when the versions differ", () => {
  assert.match(badge, /optimizerUpdates\.latest !== optimizerCurrent/);
  // The old row rendered "v1.3.2 → v1.3.2" beside the words "up to date".
  assert.doesNotMatch(badge, /optimizerUpdates\.latest \? " → " \+ escapeHTML\(optimizerUpdates\.latest\)/);
});

test("restart asks first because it drops dispatch", () => {
  assert.match(badge, /Restart the service\? Dispatch stops until Core is back and healthy\./);
  assert.match(badge, /<button class="btn btn-ghost" data-action="restart">Restart<\/button>/);
  // Restart must not carry the same weight as the primary update action.
  assert.doesNotMatch(badge, /<button class="btn" data-action="restart">/);
});

test("backup lists collapse into one section carrying the counts", () => {
  assert.match(badge, /_storageSectionHTML\(\)/);
  assert.match(badge, /<summary>Backups · \$\{escapeHTML\(parts\.join\(" · "\)\)\}<\/summary>/);
  assert.match(badge, /rollback point\$\{snapshotList\.length === 1 \? "" : "s"\}/);
  assert.match(badge, /class="storage-block"/);
});

test("local rollback points are distinguished from off-device backup", () => {
  assert.match(badge, /Local rollback points/);
  assert.match(badge, /not SD-card failure/);
  assert.match(badge, /Create rollback point/);
  assert.doesNotMatch(badge, /Skip backup for this update/);
});

test("full backups expose create, verify, download and external-storage status", () => {
  assert.match(badge, /Full backups/);
  assert.match(badge, /Create full backup/);
  assert.match(badge, /verify-backup/);
  assert.match(badge, /download>Download/);
  assert.match(badge, /do not survive SD-card failure/);
});

test("changing channel persists through the local API then forces a probe", () => {
  assert.match(badge, /_postJSON\("\/api\/version\/channel", \{ channel \}\)/);
  assert.match(badge, /this\._refresh\(true\)/);
});

test("Update dialog wires independent Optimizer and Driver history actions", () => {
  assert.match(badge, /<h3 id="ftw-upd-title">Updates<\/h3>/);
  assert.match(badge, /\/api\/components\/history\?limit=20/);
  assert.match(badge, /\/api\/components\/optimizer\/channel/);
  assert.match(badge, /\/api\/components\/optimizer\/update/);
  assert.match(badge, /\/api\/device_repository\/drivers\//);
  assert.match(badge, /\/versions/);
  assert.match(badge, /\/activate/);
});

test("optimizer fallback is visible in the global header and Update Center", () => {
  assert.match(badge, /Planner fallback active/);
  // The header mark labels itself from the same warning title it shows on
  // hover; header-status-marks.test.mjs drives the rendered states.
  assert.match(badge, /class="mark warning"[^`]*aria-label="\$\{escapeHTML\(warningTitle\)\}"/);
  assert.match(badge, /class="component-warning" role="alert"/);
  assert.match(badge, /optimizer\.fallback_reason \|\| optimizer\.health_error/);
});
