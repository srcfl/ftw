package state

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteInventoryReportsPageAndSidecarAccounting(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.db.Exec(`CREATE TABLE inventory_probe (id INTEGER PRIMARY KEY, payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		if _, err := st.db.Exec(`INSERT INTO inventory_probe(payload) VALUES (zeroblob(65536))`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`DELETE FROM inventory_probe`); err != nil {
		t.Fatal(err)
	}

	got, err := st.SQLiteInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, db := range map[string]SQLiteFileInventory{"state": got.State, "cache": got.Cache} {
		if db.PageSizeBytes <= 0 || db.AllocatedPages <= 0 {
			t.Errorf("%s page accounting = %+v", name, db)
		}
		if db.LivePages+db.FreePages != db.AllocatedPages {
			t.Errorf("%s live + free = %d, allocated = %d", name, db.LivePages+db.FreePages, db.AllocatedPages)
		}
		if db.LiveBytes+db.FreeBytes != db.AllocatedBytes {
			t.Errorf("%s live + free bytes = %d, allocated = %d", name, db.LiveBytes+db.FreeBytes, db.AllocatedBytes)
		}
		if db.FileBytes <= 0 {
			t.Errorf("%s file bytes = %d", name, db.FileBytes)
		}
	}
	if got.State.FreePages == 0 {
		t.Errorf("state freelist = 0 after deleting probe rows; inventory = %+v", got.State)
	}
}
