package statsrelay_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/statsrelay"
	"github.com/srcfl/ftw/go/internal/statsrelay/statsrelaytest"
	"github.com/srcfl/ftw/go/protocol/energywire"
	"github.com/srcfl/ftw/go/protocol/ohttp"
	"github.com/srcfl/ftw/go/protocol/reportwire"
)

var now = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

// The release names no relay and no gateway key yet, so no report goes
// through one. Once both are pinned they must be a usable pair.
func TestTheReleasePinsBothOrNeither(t *testing.T) {
	c := statsrelay.Pinned()
	switch {
	case statsrelay.RelayURL == "" && statsrelay.GatewayKey == "":
		if c != nil {
			t.Fatal("nothing pinned, yet a client")
		}
	case c == nil:
		t.Fatalf("the pinned relay %q and gateway key do not make a usable pair", statsrelay.RelayURL)
	}
}

func TestAPinIsRefusedUnlessItIsARelayAndAKey(t *testing.T) {
	k, _ := ecdh.X25519().GenerateKey(nil)
	gw, _ := ohttp.NewGateway(ohttp.Key{ID: 1, Private: k})
	key := base64.RawURLEncoding.EncodeToString(gw.Configs()[0].Marshal())
	oldRelay, oldKey := statsrelay.RelayURL, statsrelay.GatewayKey
	t.Cleanup(func() { statsrelay.RelayURL, statsrelay.GatewayKey = oldRelay, oldKey })
	set := func(relay, key string) *statsrelay.Client {
		statsrelay.RelayURL, statsrelay.GatewayKey = relay, key
		return statsrelay.Pinned()
	}
	if c := set("https://relay.example.net/ftw", key); c == nil || c.Relay() != "https://relay.example.net/ftw" {
		t.Fatal("a good pair was refused")
	}
	for name, p := range map[string][2]string{
		"no relay":         {"", key},
		"no key":           {"https://relay.example.net/ftw", ""},
		"plain http":       {"http://relay.example.net/ftw", key},
		"FTW Cloud itself": {"https://cloud.ftw.energy/v1/ohttp", key},
		"FTW Cloud, cased": {"https://Cloud.FTW.Energy/v1/ohttp", key},
		"FTW Cloud, dot":   {"https://cloud.ftw.energy./v1/ohttp", key},
		"query":            {"https://relay.example.net/ftw?box=1", key},
		"credentials":      {"https://a:b@relay.example.net/ftw", key},
		"bad key":          {"https://relay.example.net/ftw", "AAAA"},
		"padded base64":    {"https://relay.example.net/ftw", key + "="},
	} {
		if set(p[0], p[1]) != nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// The relay sees one length and no report; the gateway reads it.
func TestAReportReachesTheGatewayThroughTheRelay(t *testing.T) {
	client, gw, relay := statsrelaytest.New(t, now)
	body, _ := json.Marshal(energywire.Report{Schema: energywire.Schema, Date: "2026-10-23", GridImport: 24})
	if err := client.Send(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if got := gw.Energy(); len(got) != 1 || got[0].GridImport != 24 {
		t.Fatalf("gateway got %+v", got)
	}
	seen := relay.Bodies()[0]
	if bytes.Contains(seen, []byte("kwh")) || len(seen) != 7+32+reportwire.RequestSize+16 {
		t.Fatalf("the relay saw %d bytes", len(seen))
	}
	h := relay.Headers()[0]
	if h.Get("Content-Type") != ohttp.RequestType || h.Get("User-Agent") != "ftw-report" {
		t.Fatalf("relay headers %v", h)
	}
	if err := client.Send(context.Background(), make([]byte, reportwire.RequestSize)); err == nil {
		t.Fatal("a report too large for the padding went out")
	}
}

// The box believes only a sealed 204 from the gateway it encrypted to.
func TestOnlyTheGatewaysSealed204Counts(t *testing.T) {
	k, _ := ecdh.X25519().GenerateKey(nil)
	gw, _ := ohttp.NewGateway(ohttp.Key{ID: 1, Private: k})
	sealed := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_, ctx, err := gw.Decapsulate(body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			answer, _ := ctx.Seal(reportwire.Response(status))
			w.Header().Set("Content-Type", ohttp.ResponseType)
			_, _ = w.Write(answer)
		}
	}
	body := []byte(`{"schema":"x"}`)
	for name, h := range map[string]http.HandlerFunc{
		"relay error": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"plain 204":   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"forged body": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ohttp.ResponseType)
			_, _ = w.Write(make([]byte, 64))
		},
		"sealed 400": sealed(http.StatusBadRequest),
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusTemporaryRedirect)
		},
	} {
		srv := httptest.NewServer(h)
		if err := statsrelay.New(srv.URL, gw.Configs()[0], nil).Send(context.Background(), body); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		srv.Close()
	}
	srv := httptest.NewServer(sealed(http.StatusNoContent))
	defer srv.Close()
	if err := statsrelay.New(srv.URL, gw.Configs()[0], nil).Send(context.Background(), body); err != nil {
		t.Fatal(err)
	}
}
