package api

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/notifications"
	"github.com/srcfl/ftw/go/internal/state"
)

// pushVAPIDKey mirrors nova.Identity's signing surface for the provider.
type pushVAPIDKey struct{ priv *ecdsa.PrivateKey }

func newPushVAPIDKey(t *testing.T) *pushVAPIDKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &pushVAPIDKey{priv: priv}
}

func (k *pushVAPIDKey) PublicKeyHex() string {
	out := make([]byte, 64)
	k.priv.X.FillBytes(out[:32])
	k.priv.Y.FillBytes(out[32:])
	return hex.EncodeToString(out)
}

func (k *pushVAPIDKey) SignRawHex(msg string) (string, error) {
	hash := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, hash[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return hex.EncodeToString(sig), nil
}

type stateSubs struct{ st *state.Store }

func (s stateSubs) PushSubscriptions() ([]notifications.PushSubscription, error) {
	rows, err := s.st.PushSubscriptions()
	if err != nil {
		return nil, err
	}
	out := make([]notifications.PushSubscription, 0, len(rows))
	for _, r := range rows {
		out = append(out, notifications.PushSubscription{
			ID: r.ID, Endpoint: r.Endpoint, P256dh: r.P256dh, Auth: r.Auth,
			DeviceID: r.DeviceID, CreatedAtMs: r.CreatedAtMs,
		})
	}
	return out, nil
}

func (s stateSubs) DeletePushSubscription(id string) error {
	return s.st.DeletePushSubscription(id)
}

func pushServer(t *testing.T) (*Server, *state.Store) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	wp, err := notifications.NewWebPush(newPushVAPIDKey(t), stateSubs{st: st})
	if err != nil {
		t.Fatal(err)
	}
	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	cfg := &config.Config{}
	// The narrow rules write validates the whole document before saving,
	// so the base document has to be a valid one — as it always is live.
	cfg.Site.SmoothingAlpha = 0.3
	cfg.Fuse.MaxAmps = 16
	cfg.Fuse.Phases = 3
	cfg.Fuse.Voltage = 230
	srv := New(&Deps{
		State: st, WebPush: wp,
		Ctrl: control.NewState(0, 42, ""), CtrlMu: &ctrlMu,
		Cfg: cfg, CfgMu: &cfgMu,
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		SaveConfig: func(string, *config.Config) error { return nil },
	})
	return srv, st
}

func jsonReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func validSubscriptionBody(t *testing.T, endpoint string) string {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`,
		endpoint,
		base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(auth))
}

// The VAPID route hands out the public key and nothing else. The private
// half lives in a file; no field of any response may carry it.
func TestVAPIDRouteReturnsOnlyThePublicKey(t *testing.T) {
	srv, _ := pushServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/notifications/vapid", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 {
		t.Fatalf("response carries %d fields, want exactly public_key: %s", len(body), rr.Body.String())
	}
	var key string
	if err := json.Unmarshal(body["public_key"], &key); err != nil {
		t.Fatalf("no public_key field: %s", rr.Body.String())
	}
	raw, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(raw) != 65 || raw[0] != 0x04 {
		t.Fatalf("public_key is not an uncompressed P-256 point: %q", key)
	}
}

func TestVAPIDRouteWithoutProviderSaysSo(t *testing.T) {
	srv := New(&Deps{})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/notifications/vapid", nil))
	if rr.Code != 503 {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// Subscribing stores the row against the authenticated caller's device —
// the body has no say — and revoking that device sweeps the row.
func TestSubscriptionCRUDAndRevocationSweep(t *testing.T) {
	srv, st := pushServer(t)

	req := jsonReq(http.MethodPost, "/api/notifications/subscriptions",
		validSubscriptionBody(t, "https://push.example/p/1"))
	req = req.WithContext(apiauth.WithCaller(req.Context(), apiauth.Caller{
		Subject: "app:phone1", Kind: apiauth.KindApp, Role: apiauth.RoleOwner,
	}))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("subscribe = %d: %s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got["id"] == "" {
		t.Fatalf("response = %s", rr.Body.String())
	}

	rows, err := st.PushSubscriptions()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if rows[0].DeviceID != "phone1" {
		t.Fatalf("device = %q, want phone1 (from the caller, not the body)", rows[0].DeviceID)
	}

	// A second phone subscribes; deleting by id removes only its row.
	req2 := jsonReq(http.MethodPost, "/api/notifications/subscriptions",
		validSubscriptionBody(t, "https://push.example/p/2"))
	req2 = req2.WithContext(apiauth.WithCaller(req2.Context(), apiauth.Caller{
		Subject: "app:phone2", Kind: apiauth.KindApp, Role: apiauth.RoleOwner,
	}))
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != 200 {
		t.Fatalf("second subscribe = %d", rr2.Code)
	}

	del := httptest.NewRequest(http.MethodDelete, "/api/notifications/subscriptions/"+got["id"], nil)
	rrDel := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrDel, del)
	if rrDel.Code != 200 {
		t.Fatalf("delete = %d", rrDel.Code)
	}
	rows, _ = st.PushSubscriptions()
	if len(rows) != 1 || rows[0].DeviceID != "phone2" {
		t.Fatalf("rows after delete = %+v", rows)
	}

	// The revocation path's sweep: phone2 is revoked, its row goes too.
	if n, err := st.DeletePushSubscriptionsByDevice("phone2"); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if rows, _ := st.PushSubscriptions(); len(rows) != 0 {
		t.Fatalf("rows after revocation = %+v", rows)
	}
}

func TestSubscribeRefusesBadInput(t *testing.T) {
	srv, _ := pushServer(t)
	for name, body := range map[string]string{
		"http endpoint":  validSubscriptionBody(t, "http://push.example/p/1"),
		"not a url":      validSubscriptionBody(t, "not a url"),
		"garbage keys":   `{"endpoint":"https://push.example/x","keys":{"p256dh":"AA","auth":"BB"}}`,
		"missing keys":   `{"endpoint":"https://push.example/x"}`,
		"empty document": `{}`,
	} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, jsonReq(http.MethodPost,
			"/api/notifications/subscriptions", body))
		if rr.Code != 400 {
			t.Fatalf("%s: status = %d, want 400", name, rr.Code)
		}
	}
}

// The narrow rules write: upserts per type, keeps what it never mentioned,
// flips the master switch, and reaches the live service through the same
// apply path a file edit uses.
func TestRulesPutUpsertsWithoutWipingNeighbours(t *testing.T) {
	srv, _ := pushServer(t)

	// Seed a config whose ntfy rule the write must not disturb.
	srv.deps.CfgMu.Lock()
	srv.deps.Cfg.Notifications = &config.Notifications{
		Enabled: false,
		Events: []config.NotificationRule{
			{Type: notifications.EventDriverOffline, Enabled: true, ThresholdS: 600, Priority: 4},
		},
	}
	srv.deps.CfgMu.Unlock()

	body := `{"enabled":true,"events":[{"type":"charging.session_complete","enabled":true,"priority":3}]}`
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, jsonReq(http.MethodPut,
		"/api/notifications/rules", body))
	if rr.Code != 200 {
		t.Fatalf("rules put = %d: %s", rr.Code, rr.Body.String())
	}

	srv.deps.CfgMu.RLock()
	notif := srv.deps.Cfg.Notifications
	srv.deps.CfgMu.RUnlock()
	if notif == nil || !notif.Enabled {
		t.Fatalf("master switch did not flip: %+v", notif)
	}
	var offline, complete *config.NotificationRule
	for i := range notif.Events {
		switch notif.Events[i].Type {
		case notifications.EventDriverOffline:
			offline = &notif.Events[i]
		case notifications.PushChargingSessionComplete:
			complete = &notif.Events[i]
		}
	}
	if offline == nil || !offline.Enabled || offline.ThresholdS != 600 {
		t.Fatalf("the untouched rule moved: %+v", offline)
	}
	if complete == nil || !complete.Enabled || complete.Priority != 3 {
		t.Fatalf("the submitted rule did not land: %+v", complete)
	}
}

func TestRulesPutRefusesUnknownTypes(t *testing.T) {
	srv, _ := pushServer(t)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, jsonReq(http.MethodPut,
		"/api/notifications/rules",
		`{"events":[{"type":"charging.someday","enabled":true}]}`))
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "charging.someday") {
		t.Fatalf("the refusal does not name the type: %s", rr.Body.String())
	}
}

// Before anything is stored, the read answers the full disabled default
// set — the toggles the app draws — and writes nothing while doing it.
func TestRulesGetShowsDefaultsAndStoresNothing(t *testing.T) {
	srv, _ := pushServer(t)
	srv.deps.SaveConfig = func(string, *config.Config) error {
		t.Error("a GET wrote the config")
		return nil
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/notifications/rules", nil))
	if rr.Code != 200 {
		t.Fatalf("rules get = %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Enabled bool                      `json:"enabled"`
		Events  []config.NotificationRule `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("nothing is stored, yet the master switch reads on")
	}
	defaults := notifications.DefaultRules()
	if len(got.Events) != len(defaults) {
		t.Fatalf("%d events, want the full default set of %d", len(got.Events), len(defaults))
	}
	for i, e := range got.Events {
		if e.Type != defaults[i].Type {
			t.Fatalf("event %d is %q, want %q", i, e.Type, defaults[i].Type)
		}
		if e.Enabled {
			t.Fatalf("default rule %q came back enabled", e.Type)
		}
	}

	srv.deps.CfgMu.RLock()
	stored := srv.deps.Cfg.Notifications
	srv.deps.CfgMu.RUnlock()
	if stored != nil {
		t.Fatalf("the GET stored a notifications section: %+v", stored)
	}
}

// After a write, the read answers what was stored — byte for byte the
// document the PUT itself answered, because both come from one builder.
func TestRulesGetAfterPutShowsWhatWasStored(t *testing.T) {
	srv, _ := pushServer(t)
	put := httptest.NewRecorder()
	srv.Handler().ServeHTTP(put, jsonReq(http.MethodPut, "/api/notifications/rules",
		`{"enabled":true,"events":[{"type":"update.installed","enabled":true}]}`))
	if put.Code != 200 {
		t.Fatalf("rules put = %d: %s", put.Code, put.Body.String())
	}

	get := httptest.NewRecorder()
	srv.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/notifications/rules", nil))
	if get.Code != 200 {
		t.Fatalf("rules get = %d: %s", get.Code, get.Body.String())
	}
	if get.Body.String() != put.Body.String() {
		t.Fatalf("GET = %s, want the PUT's own answer %s", get.Body.String(), put.Body.String())
	}
}

// First touch with no notifications section seeds the full disabled rule
// set, so Settings later shows every event rather than only the one the
// app enabled.
func TestRulesPutSeedsDefaultsOnFirstTouch(t *testing.T) {
	srv, _ := pushServer(t)
	body := `{"enabled":true,"events":[{"type":"update.installed","enabled":true}]}`
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, jsonReq(http.MethodPut,
		"/api/notifications/rules", body))
	if rr.Code != 200 {
		t.Fatalf("rules put = %d: %s", rr.Code, rr.Body.String())
	}
	srv.deps.CfgMu.RLock()
	notif := srv.deps.Cfg.Notifications
	srv.deps.CfgMu.RUnlock()
	if notif == nil || len(notif.Events) < len(notifications.DefaultRules()) {
		t.Fatalf("defaults not seeded: %+v", notif)
	}
	enabled := 0
	for _, e := range notif.Events {
		if e.Enabled {
			enabled++
			if e.Type != notifications.PushUpdateInstalled {
				t.Fatalf("seeded rule %q came up enabled", e.Type)
			}
		}
	}
	if enabled != 1 {
		t.Fatalf("%d rules enabled, want exactly the submitted one", enabled)
	}
}

// A box that saved rules before a release added new kinds must still be
// offered the new ones: stored entries untouched, missing known types
// appended at their disabled defaults. The real regression: the first
// household to ever save a rule could never see a kind added later.
func TestRulesGetOffersKindsAddedAfterTheConfigWasSaved(t *testing.T) {
	srv, _ := pushServer(t)
	srv.deps.Cfg.Notifications = &config.Notifications{
		Enabled: true,
		Events: []config.NotificationRule{
			{Type: "driver_offline", Enabled: true, ThresholdS: 120, Priority: 4, CooldownS: 600},
		},
	}
	srv.deps.SaveConfig = func(string, *config.Config) error {
		t.Error("the offer wrote the config — it must be a view, not a migration")
		return nil
	}

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/notifications/rules", nil))
	if rr.Code != 200 {
		t.Fatalf("rules get = %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Enabled bool                      `json:"enabled"`
		Events  []config.NotificationRule `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	byType := map[string]config.NotificationRule{}
	for _, e := range got.Events {
		byType[e.Type] = e
	}
	// The stored choice survives byte for byte.
	stored := byType["driver_offline"]
	if !stored.Enabled || stored.ThresholdS != 120 || stored.CooldownS != 600 {
		t.Fatalf("the stored rule was rewritten: %+v", stored)
	}
	// The kinds the release added are offered, disabled.
	for _, kind := range []string{
		"charging.session_complete", "charging.interrupted", "update.installed",
		"driver.offline", "fuse.over_limit",
	} {
		rule, ok := byType[kind]
		if !ok {
			t.Fatalf("kind %s is not offered to a box with a stored config", kind)
		}
		if rule.Enabled {
			t.Fatalf("kind %s arrived enabled — new kinds must arrive off", kind)
		}
	}
	// And the stored slice itself is untouched.
	if len(srv.deps.Cfg.Notifications.Events) != 1 {
		t.Fatalf("the GET grew the stored config to %d events", len(srv.deps.Cfg.Notifications.Events))
	}
}
