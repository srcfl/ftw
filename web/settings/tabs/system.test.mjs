// node --test web/settings/tabs/system.test.mjs

import { describe, it } from "node:test";
import assert from "node:assert/strict";

globalThis.window = {};
await import("./system.js");
const tab = globalThis.window.FTWSettings.tabs.system;
const { fmtBytes, storageInventoryHTML } = tab._pure;

function esc(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

describe("storage inventory", () => {
  it("renders the SQLite, artifact and dry-run budget breakdown", () => {
    const html = storageInventoryHTML({
      databases: {
        state: {file_bytes: 1000, wal_bytes: 20, shm_bytes: 10, live_bytes: 800, free_bytes: 200},
        cache: {file_bytes: 100, wal_bytes: 5, live_bytes: 80, free_bytes: 20},
      },
      files: {
        parquet: {bytes: 500, files: 2, diagnostics_bytes: 100, retention_days: 0, on_device: true},
        recovery_snapshot: {bytes: 40, files: 1, on_device: true},
        rollback_snapshots: {bytes: 60, files: 2, on_device: true},
        full_backups: {bytes: 70, files: 1, on_device: false},
        other_data: {bytes: 30, on_device: true},
      },
      filesystem: {available_bytes: 5000},
      advisor: {
        status: "watch", budget_bytes: 1500, managed_bytes: 1400,
        filesystem_reserve_bytes: 500, read_only: true, candidates: [],
      },
      maintenance: {},
    }, esc);

    assert.match(html, /state\.db/);
    assert.match(html, /free pages/);
    assert.match(html, /Cold Parquet/);
    assert.match(html, /retention unbounded/);
    assert.match(html, /Full backups/);
    assert.match(html, /external/);
    assert.match(html, /Dry-run target/);
    assert.match(html, /no files, retention settings or SQLite pages are changed/i);
  });

  it("escapes server-provided advisor text", () => {
    const html = storageInventoryHTML({
      files: {parquet: {}, recovery_snapshot: {}, rollback_snapshots: {}, full_backups: {}, other_data: {}},
      advisor: {
        status: "action_needed", candidates: [{
          category: "parquet<script>", action: "<img src=x onerror=alert(1)>", would_consider: true,
        }],
      },
    }, esc);
    assert.ok(!html.includes("<script>"));
    assert.ok(!html.includes("<img"));
    assert.match(html, /parquet&lt;script&gt;/);
  });

  it("formats zero bytes explicitly", () => {
    assert.equal(fmtBytes(0), "0 B");
    assert.equal(fmtBytes(1024), "1.0 KB");
  });
});

describe("system tab", () => {
  it("offers an explicit storage refresh instead of polling inventory", () => {
    const html = tab.render();
    assert.match(html, /id="sys-storage-inventory"/);
    assert.match(html, /id="sys-storage-refresh"/);
  });
});
