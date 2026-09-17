package state

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func differentForecastGzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z, err := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	z.Name = "older-encoder"
	if _, err := z.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestForecastArchiveKeepsIDsAcrossCompressionChanges(t *testing.T) {
	s := openForecastArchive(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Hour).UnixMilli()
	issue := archiveIssue("before-upgrade", now, now)
	issue.Models = []forecasting.ModelState{{Name: "pv", Version: "v1", Quality: forecasting.ModelQualityWarm,
		UpdatedAtMS: now, State: json.RawMessage(`{"weights":[1,2,3],"samples":400}`)}}
	if err := s.SaveForecastIssue(ctx, issue); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareForecastIssue(issue)
	if err != nil {
		t.Fatal(err)
	}
	state := prepared.modelStates[0]
	oldModel := differentForecastGzip(t, issue.Models[0].State)
	rawIssue, err := json.Marshal(prepared.issue)
	if err != nil {
		t.Fatal(err)
	}
	oldIssue := differentForecastGzip(t, rawIssue)
	if bytes.Equal(oldModel, state.payload) || bytes.Equal(oldIssue, prepared.payload) {
		t.Fatal("fixture must use different compression")
	}
	if _, err := s.db.Exec(`UPDATE forecast_model_states SET payload=? WHERE id=?`, oldModel, state.id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE forecast_issues SET payload=? WHERE id=?`, oldIssue, issue.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveForecastIssue(ctx, issue); err != nil {
		t.Fatalf("retry after encoder change: %v", err)
	}
	issue.ID = "after-upgrade"
	if err := s.SaveForecastIssue(ctx, issue); err != nil {
		t.Fatalf("new forecast referencing existing model: %v", err)
	}
	loaded, err := s.LoadForecastModelState(ctx, state.id)
	if err != nil || !bytes.Equal(loaded, issue.Models[0].State) {
		t.Fatalf("model changed: %s %v", loaded, err)
	}
	var count int
	var payload []byte
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM forecast_model_states`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("model count %d: %v", count, err)
	}
	if err := s.db.QueryRow(`SELECT payload FROM forecast_model_states WHERE id=?`, state.id).Scan(&payload); err != nil || !bytes.Equal(payload, oldModel) {
		t.Fatal("rewrote the existing model")
	}
	if err := s.db.QueryRow(`SELECT payload FROM forecast_issues WHERE id='before-upgrade'`).Scan(&payload); err != nil || !bytes.Equal(payload, oldIssue) {
		t.Fatal("rewrote the existing forecast")
	}
}

func TestForecastArchiveStillRejectsChangedOrCorruptModel(t *testing.T) {
	for _, kind := range []string{"changed", "truncated", "too-large"} {
		t.Run(kind, func(t *testing.T) {
			s := openForecastArchive(t)
			now := time.Now().Add(-time.Hour).UnixMilli()
			issue := archiveIssue("one", now, now)
			issue.Models = []forecasting.ModelState{{Name: "pv", Version: "v1", Quality: forecasting.ModelQualityWarm,
				UpdatedAtMS: now, State: json.RawMessage(`{"weights":[1,2,3]}`)}}
			if err := s.SaveForecastIssue(context.Background(), issue); err != nil {
				t.Fatal(err)
			}
			payload := differentForecastGzip(t, []byte(`{"weights":[4,5,6]}`))
			if kind == "truncated" {
				payload = payload[:len(payload)-4]
			}
			if kind == "too-large" {
				payload = differentForecastGzip(t, bytes.Repeat([]byte("x"), forecasting.MaxModelStateBytes+1))
			}
			if _, err := s.db.Exec(`UPDATE forecast_model_states SET payload=?`, payload); err != nil {
				t.Fatal(err)
			}
			issue.ID = "two"
			if err := s.SaveForecastIssue(context.Background(), issue); err == nil {
				t.Fatal("accepted invalid existing model")
			}
			var n int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM forecast_issues`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("failed save was not atomic: %d %v", n, err)
			}
		})
	}
}
