package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func configContentionStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := openRaw(path)
	if err != nil {
		t.Fatal("open temporary state database")
	}
	s := &Store{db: db}
	t.Cleanup(func() { _ = s.db.Close() })
	if _, err := db.Exec(`CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal("create temporary configuration table")
	}
	return s, path
}

func configContentionValue(t *testing.T) string {
	t.Helper()
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal("create temporary test value")
	}
	return hex.EncodeToString(value[:])
}

func configContentionDocument(t *testing.T, owner, name string) []byte {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"config": map[string]any{"drivers": []map[string]string{{"name": name, "credential_owner": owner}}},
	})
	if err != nil {
		t.Fatal("encode temporary settings")
	}
	return document
}

func configContentionWriter(t *testing.T, path string) (*sql.Tx, func()) {
	t.Helper()
	db, err := openRaw(path)
	if err != nil {
		t.Fatal("open independent writer")
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal("begin independent write")
	}
	if _, err := tx.Exec(`UPDATE config SET value=value WHERE 0`); err != nil {
		t.Fatal("reserve independent writer")
	}
	closeWriter := func() { _ = tx.Rollback(); _ = db.Close() }
	t.Cleanup(closeWriter)
	return tx, closeWriter
}

func TestSaveDriverSecretWaitsForWriter(t *testing.T) {
	for _, outcome := range []string{"commit_rename", "rollback_rename", "no_settings_row"} {
		t.Run(outcome, func(t *testing.T) {
			s, path := configContentionStore(t)
			owner, oldName, newName := configContentionValue(t), configContentionValue(t), configContentionValue(t)
			oldSecret, nextSecret := configContentionValue(t), configContentionValue(t)
			if outcome != "no_settings_row" {
				if _, err := s.SaveConfiguration(configContentionDocument(t, owner, oldName), 0, nil); err != nil {
					t.Fatal("save initial settings")
				}
				if err := s.SaveDriverSecret(owner, "refresh_token", oldSecret); err != nil {
					t.Fatal("save initial secret")
				}
			}
			tx, closeWriter := configContentionWriter(t, path)
			if outcome != "no_settings_row" {
				raw, err := json.Marshal(Configuration{Version: 1, Revision: 2, Document: configContentionDocument(t, owner, newName)})
				if err != nil {
					t.Fatal("encode renamed settings")
				}
				if _, err := tx.Exec(`UPDATE config SET value=? WHERE key=?`, string(raw), configurationKey); err != nil {
					t.Fatal("stage concurrent rename")
				}
			}
			started, done := make(chan struct{}), make(chan error, 1)
			go func() { close(started); done <- s.SaveDriverSecret(owner, "refresh_token", nextSecret) }()
			<-started
			select {
			case err := <-done:
				var code interface{ Code() int }
				if errors.As(err, &code) && code.Code()&0xff == 5 {
					t.Fatal("secret read-to-write upgrade returned SQLITE_BUSY before the writer released its transaction")
				}
				t.Fatal("secret write returned before the competing writer released its transaction")
			case <-time.After(100 * time.Millisecond):
			}
			// WAL readers must remain usable while this writer waits.
			if _, _, err := s.Configuration(); err != nil {
				t.Fatal("settings read blocked by a waiting writer")
			}
			if outcome == "rollback_rename" {
				if err := tx.Rollback(); err != nil {
					t.Fatal("roll back competing rename")
				}
			} else if err := tx.Commit(); err != nil {
				t.Fatal("commit competing writer")
			}
			closeWriter()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("secret write failed after the competing writer released its transaction")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("secret write did not finish after writer release")
			}
			if err := s.db.Close(); err != nil {
				t.Fatal("close saved state")
			}
			reopened, err := openRaw(path)
			if err != nil {
				t.Fatal("reopen saved state")
			}
			s.db = reopened
			if got, ok := s.LoadConfig(DriverSecretKey(owner, "refresh_token")); !ok || got != nextSecret {
				t.Fatal("rotated owner secret did not survive reopen")
			}
			if outcome == "no_settings_row" {
				if _, found, err := s.Configuration(); err != nil || found {
					t.Fatal("writer reservation created a settings document")
				}
				return
			}
			name, untouched, untouchedSecret := newName, oldName, oldSecret
			if outcome == "rollback_rename" {
				name, untouched, untouchedSecret = oldName, newName, ""
			}
			if got, ok := s.LoadConfig(DriverSecretKey(name, "refresh_token")); !ok || got != nextSecret {
				t.Fatal("rotated alias secret does not match the committed binding")
			}
			if got, _ := s.LoadConfig(DriverSecretKey(untouched, "refresh_token")); got != untouchedSecret {
				t.Fatal("rotation changed an alias outside the committed binding")
			}
			if got, ok := s.LoadConfig(DriverSecretLegacyHashKey(owner, "refresh_token")); !ok || got != DriverSecretValueHash(nextSecret) {
				t.Fatal("legacy binding did not survive reopen")
			}
		})
	}
}

func TestDurableConfigWriteWaitsBeforeReading(t *testing.T) {
	s, path := configContentionStore(t)
	tx, closeWriter := configContentionWriter(t, path)
	entered, done := make(chan struct{}, 1), make(chan error, 1)
	go func() {
		done <- s.durableConfigWrite(func(tx *sql.Tx) error {
			entered <- struct{}{}
			var count int
			return tx.QueryRow(`SELECT count(*) FROM config`).Scan(&count)
		})
	}()
	select {
	case <-entered:
		t.Fatal("callback read before it owned the writer reservation")
	case <-done:
		t.Fatal("write returned while the competing transaction held the writer")
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("release independent writer")
	}
	closeWriter()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("write failed after writer release")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write did not finish after release")
	}
	select {
	case <-entered:
	default:
		t.Fatal("write callback never ran")
	}
}

func TestDurableConfigWriteContentionDeadline(t *testing.T) {
	s, path := configContentionStore(t)
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	_, closeWriter := configContentionWriter(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	called := false
	started := time.Now()
	err := s.durableConfigWriteContext(ctx, func(tx *sql.Tx) error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("writer wait did not return the caller deadline")
	}
	if called {
		t.Error("expired writer wait ran the callback")
	}
	if time.Since(started) > time.Second {
		t.Error("writer wait outlived the caller deadline")
	}
	closeWriter()
	assertConfigConnectionPolicy(t, s)
	if err := s.SaveConfigValues(map[string]string{configContentionValue(t): configContentionValue(t)}); err != nil {
		t.Fatal("cancelled writer left the connection unusable")
	}
}

func assertConfigConnectionPolicy(t *testing.T, s *Store) {
	t.Helper()
	var synchronous, busyMS int
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal("read restored synchronous policy")
	}
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyMS); err != nil {
		t.Fatal("read restored busy policy")
	}
	if synchronous != 1 || busyMS != 5000 {
		t.Fatal("durable writer changed the pooled connection policy")
	}
}

func TestDurableConfigWriteRollbackAndPolicy(t *testing.T) {
	s, path := configContentionStore(t)
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	key, value := configContentionValue(t), configContentionValue(t)
	injected := errors.New("rollback requested by test")
	calls := 0
	err := s.durableConfigWrite(func(tx *sql.Tx) error {
		calls++
		var synchronous int
		if err := tx.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 2 {
			t.Error("durable callback did not use FULL synchronous")
		}
		if _, err := tx.Exec(`INSERT INTO config(key,value) VALUES(?,?)`, key, value); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) || calls != 1 {
		t.Fatal("failed callback was retried or lost its error")
	}
	if _, found := s.LoadConfig(key); found {
		t.Fatal("failed callback left a partial value")
	}
	assertConfigConnectionPolicy(t, s)
	// A second handle must be able to take the writer after rollback.
	tx, closeWriter := configContentionWriter(t, path)
	if err := tx.Commit(); err != nil {
		t.Fatal("failed callback retained the writer")
	}
	closeWriter()
	if err := s.SaveConfigValues(map[string]string{key: value}); err != nil {
		t.Fatal("save after rollback")
	}
	assertConfigConnectionPolicy(t, s)
}

func TestDurableConfigWriteContentionBudget(t *testing.T) {
	s, path := configContentionStore(t)
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	_, closeWriter := configContentionWriter(t, path)
	called := false
	start := time.Now()
	err := s.durableConfigWrite(func(*sql.Tx) error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatal("persistent contention did not stop before the callback at its call deadline")
	}
	if elapsed := time.Since(start); elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatal("persistent contention did not respect the five-second call budget")
	}
	closeWriter()
	assertConfigConnectionPolicy(t, s)
	if err := s.SaveConfigValues(map[string]string{configContentionValue(t): configContentionValue(t)}); err != nil {
		t.Fatal("write failed after a timed-out reservation")
	}
}

func TestSaveConfigurationContentionKeepsRevisionConflict(t *testing.T) {
	s, path := configContentionStore(t)
	document := configContentionDocument(t, configContentionValue(t), configContentionValue(t))
	if _, err := s.SaveConfiguration(document, 0, nil); err != nil {
		t.Fatal("save initial settings revision")
	}
	tx, closeWriter := configContentionWriter(t, path)
	started, done := make(chan struct{}, 2), make(chan error, 2)
	for range 2 {
		go func() {
			started <- struct{}{}
			_, err := s.SaveConfiguration(document, 1, nil)
			done <- err
		}()
	}
	<-started
	<-started
	select {
	case <-done:
		t.Fatal("settings save returned before competing writer release")
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("release competing settings writer")
	}
	closeWriter()
	saved, conflicts := 0, 0
	for range 2 {
		select {
		case err := <-done:
			switch {
			case err == nil:
				saved++
			case errors.Is(err, ErrConfigurationConflict):
				conflicts++
			default:
				t.Fatal("settings contention returned a storage error instead of a revision conflict")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("settings save did not finish after writer release")
		}
	}
	if saved != 1 || conflicts != 1 {
		t.Fatal("concurrent saves did not preserve optimistic revision checks")
	}
	if cfg, found, err := s.Configuration(); err != nil || !found || cfg.Revision != 2 {
		t.Fatal("concurrent settings save changed the revision incorrectly")
	}
}

func TestDurableConfigWriteDoesNotRetryOtherErrors(t *testing.T) {
	s, _ := configContentionStore(t)
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	if _, err := s.db.Exec(`DROP TABLE config`); err != nil {
		t.Fatal("remove temporary configuration table")
	}
	called := false
	start := time.Now()
	err := s.durableConfigWrite(func(*sql.Tx) error { called = true; return nil })
	if err == nil || called || time.Since(start) > time.Second {
		t.Fatal("a non-contention error was retried or ran the callback")
	}
	assertConfigConnectionPolicy(t, s)
}
