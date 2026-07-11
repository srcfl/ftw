package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NovaDER records the Nova-assigned der_id for a local (device_id, der_type)
// pair. Populated by the nova provisioning flow on startup; consulted by
// the publisher only to surface the Nova identity in /api/nova/status and
// by the reconcile loop to detect drift.
type NovaDER struct {
	DeviceID string
	DerType  string // "meter" | "pv" | "battery" | "ev"
	DerName  string // human name we chose (e.g. "solis-battery")
	DerID    string // Nova-generated "der-{uuid7}"
	SyncedMs int64
}

// UpsertNovaDER records or refreshes the Nova der_id for a local DER. Idempotent.
func (s *Store) UpsertNovaDER(d NovaDER) error {
	if d.DeviceID == "" || d.DerType == "" {
		return errors.New("UpsertNovaDER: device_id and der_type required")
	}
	if d.SyncedMs == 0 {
		d.SyncedMs = time.Now().UnixMilli()
	}
	_, err := s.db.Exec(`
		INSERT INTO nova_ders (device_id, der_type, der_name, der_id, synced_ms)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (device_id, der_type) DO UPDATE SET
			der_name  = excluded.der_name,
			der_id    = excluded.der_id,
			synced_ms = excluded.synced_ms`,
		d.DeviceID, d.DerType, d.DerName, d.DerID, d.SyncedMs)
	if err != nil {
		return fmt.Errorf("upsert nova_der: %w", err)
	}
	return nil
}

// GetNovaDER looks up the Nova mapping for one local DER. Returns nil
// if not yet provisioned.
func (s *Store) GetNovaDER(deviceID, derType string) *NovaDER {
	row := s.db.QueryRow(`SELECT device_id, der_type, der_name, der_id, synced_ms
		FROM nova_ders WHERE device_id = ? AND der_type = ?`, deviceID, derType)
	var d NovaDER
	if err := row.Scan(&d.DeviceID, &d.DerType, &d.DerName, &d.DerID, &d.SyncedMs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return nil
	}
	return &d
}

// ListNovaDERs returns every known Nova mapping, ordered by device/type.
func (s *Store) ListNovaDERs() ([]NovaDER, error) {
	rows, err := s.db.Query(`SELECT device_id, der_type, der_name, der_id, synced_ms
		FROM nova_ders ORDER BY device_id, der_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NovaDER, 0)
	for rows.Next() {
		var d NovaDER
		if err := rows.Scan(&d.DeviceID, &d.DerType, &d.DerName, &d.DerID, &d.SyncedMs); err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteNovaDER removes a Nova mapping (e.g. after a driver is removed
// from config and the operator wants to forget the stale provisioning).
func (s *Store) DeleteNovaDER(deviceID, derType string) error {
	_, err := s.db.Exec(`DELETE FROM nova_ders WHERE device_id = ? AND der_type = ?`,
		deviceID, derType)
	return err
}

// InferDerKinds returns the clean DER kinds (meter/pv/battery/ev/v2x_charger) that a
// driver has emitted at least one telemetry sample for, inferred from
// the long-format TS DB (ts_samples keyed on {kind}_w metrics). Used by
// the nova-claim CLI to decide which DERs to register under one device
// when the operator doesn't want to name them individually.
//
// Empty slice means the driver has never emitted anything — probably
// means forty-two-watts hasn't been run long enough for the driver to
// connect. The CLI surfaces a hint to that effect.
func (s *Store) InferDerKinds(driver string) []string {
	out := make([]string, 0, 5)
	for _, k := range []string{"meter", "pv", "battery", "ev", "v2x_charger"} {
		sample, err := s.LatestSample(driver, k+"_w")
		if err == nil && sample.TsMs > 0 {
			out = append(out, k)
		}
	}
	return out
}
