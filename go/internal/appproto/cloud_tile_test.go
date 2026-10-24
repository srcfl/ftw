package appproto

import (
	"bytes"
	"context"
	"testing"

	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// ledgerSite is a history provider as the cloud link reads it.
type ledgerSite struct{ HistoryProvider }

func (s ledgerSite) HistoryWindow(ctx context.Context, names []string, stepMs, fromMs, toMs int64) (map[string][]HistorySample, error) {
	return s.Window(ctx, names, stepMs, fromMs, toMs)
}

// A closed tile FTW Cloud gets is the tile the box serves over Noise for the
// same series: the same id, etag and bytes, holes included, read through the
// two real paths.
func TestACloudTileIsTheNoiseTile(t *testing.T) {
	start := tileStartFor(ResN5m, 1_760_000_000_000-24*3_600_000)
	span := ResolutionTileSpanMs[ResN5m]
	ledger := &fakeLedger{
		values: map[string]float64{"grid_w": 1200.4, "pv_w": -800, "battery_w": 300, "load_w": 1700.6},
		holes:  map[int64]bool{start + 5*300_000: true, start + 90*300_000: true},
	}
	h := &Handler{cfg: Config{History: ledger}}
	plan := planHistoryQuery(ResN5m, start, start+span, defaultMaxPoints)
	noise, err := h.buildTile(context.Background(), cloudwire.TileSeries, plan, plan.tiles[0], start+2*span)
	if err != nil {
		t.Fatal(err)
	}
	cloud, err := cloudlink.PackTile(context.Background(), ledgerSite{ledger}, start, start, start+span)
	if err != nil {
		t.Fatal(err)
	}
	if noise.TileID != cloud.TileID || noise.Etag != cloud.Etag || !bytes.Equal(noise.Data, cloud.Data) ||
		noise.Partial || cloud.Partial || noise.StepMs != cloud.StepMs {
		t.Fatalf("Noise %s %s, FTW Cloud %s %s", noise.TileID, noise.Etag, cloud.TileID, cloud.Etag)
	}
}
