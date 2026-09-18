package api

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/configreload"
	"github.com/srcfl/ftw/go/internal/notifications"
)

// The web-push routes. The provider itself lives in internal/notifications;
// these handlers own only the HTTP shapes: the public key, the subscription
// rows, and the narrow rules write that lets the app enable an event without
// carrying the whole configuration document over a relay.

// GET /api/notifications/vapid — the applicationServerKey the app hands to
// PushManager.subscribe. The public key and nothing else: the private half
// lives in a file beside state.db and no route returns it.
func (s *Server) handleNotificationsVAPID(w http.ResponseWriter, r *http.Request) {
	if s.deps.WebPush == nil {
		writeJSON(w, 503, map[string]string{"error": "web push unavailable"})
		return
	}
	key, err := s.deps.WebPush.PublicKeyB64()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"public_key": key})
}

// POST /api/notifications/subscriptions — store one browser subscription.
// The body is what PushManager.subscribe returned; the device it belongs to
// is whoever the session authenticated, never a field the body could lie in.
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 503, map[string]string{"error": "no state store"})
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	endpoint, err := url.Parse(strings.TrimSpace(req.Endpoint))
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		// Push services live on https origins, and this box will POST
		// encrypted payloads at whatever is stored here — so nothing
		// else may be.
		writeJSON(w, 400, map[string]string{"error": "endpoint must be an https URL"})
		return
	}
	if err := notifications.ValidateSubscriptionKeys(req.Keys.P256dh, req.Keys.Auth); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	id, err := s.deps.State.SavePushSubscription(
		endpoint.String(), req.Keys.P256dh, req.Keys.Auth, callerDeviceID(r))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.pokePushResync()
	writeJSON(w, 200, map[string]string{"id": id})
}

// DELETE /api/notifications/subscriptions/{id} — forget one subscription.
// Idempotent, because the caller wanted it gone and it is.
func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 503, map[string]string{"error": "no state store"})
		return
	}
	if err := s.deps.State.DeletePushSubscription(r.PathValue("id")); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.pokePushResync()
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// effectiveNotificationRules is the rules document as the app should see
// it: the stored section, deep-copied so the caller may edit its events
// slice, or the full disabled default set when nothing is stored yet — so
// Settings shows every event rather than none. GET answers it untouched;
// PUT mutates its copy and saves. One builder, so the two verbs can never
// show different documents.
func (s *Server) effectiveNotificationRules() config.Notifications {
	s.deps.CfgMu.RLock()
	defer s.deps.CfgMu.RUnlock()
	notif := config.Notifications{}
	if s.deps.Cfg.Notifications != nil {
		notif = *s.deps.Cfg.Notifications
		notif.Events = append([]config.NotificationRule(nil), notif.Events...)
		// A box that chose its rules before a release added new kinds must
		// still be offered them: the stored entries win untouched, and any
		// known type the stored list lacks is appended at its default,
		// disabled. Without this, the first box to ever save a rule was
		// also the last box that could ever see a new one.
		have := make(map[string]bool, len(notif.Events))
		for _, rule := range notif.Events {
			have[rule.Type] = true
		}
		for _, rule := range notifications.DefaultRules() {
			if !have[rule.Type] {
				notif.Events = append(notif.Events, rule)
			}
		}
	} else {
		notif.Events = notifications.DefaultRules()
	}
	return notif
}

// writeNotificationRules answers the rules document. Both verbs of the
// route speak exactly this shape.
func writeNotificationRules(w http.ResponseWriter, notif config.Notifications) {
	writeJSON(w, 200, map[string]any{
		"enabled": notif.Enabled,
		"events":  notif.Events,
	})
}

// GET /api/notifications/rules — the same document the PUT answers, read
// without writing anything. The app's toggles must show the current rules
// before anyone flips one, and showing them is a read: it cannot cost the
// step-up ceremony the write rightly does, and it must not store the
// defaults it renders — a GET that writes is a lie.
func (s *Server) handleNotificationsRulesGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Cfg == nil || s.deps.CfgMu == nil {
		writeJSON(w, 503, map[string]string{"error": "configuration is not readable here"})
		return
	}
	writeNotificationRules(w, s.effectiveNotificationRules())
}

// PUT /api/notifications/rules — the narrow rules write.
//
// POST /api/config is refused over the passthrough because its body replaces
// the whole document, and a phone's idea of the document may be a year old.
// This is the surgical alternative: each submitted entry upserts the rule of
// its own type, types not mentioned keep whatever they had, and the optional
// top-level enabled flips the master switch. Nothing here can wipe a setting
// its sender never knew about.
func (s *Server) handleNotificationsRulesPut(w http.ResponseWriter, r *http.Request) {
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	if s.deps.Cfg == nil || s.deps.CfgMu == nil || s.deps.SaveConfig == nil {
		writeJSON(w, 503, map[string]string{"error": "configuration is not writable here"})
		return
	}
	var req struct {
		Enabled *bool                     `json:"enabled,omitempty"`
		Events  []config.NotificationRule `json:"events"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	known := map[string]bool{}
	for _, t := range notifications.KnownRuleTypes() {
		known[t] = true
	}
	for _, e := range req.Events {
		if !known[e.Type] {
			writeJSON(w, 400, map[string]string{"error": "unknown event type: " + e.Type})
			return
		}
	}

	// The same document the GET renders — first touch seeds the full
	// disabled rule set, so the operator's later visit to Settings sees
	// every event, not just this one.
	s.deps.CfgMu.RLock()
	newCfg := *s.deps.Cfg
	s.deps.CfgMu.RUnlock()
	notif := s.effectiveNotificationRules()

	for _, e := range req.Events {
		replaced := false
		for i := range notif.Events {
			if notif.Events[i].Type == e.Type {
				notif.Events[i] = e
				replaced = true
				break
			}
		}
		if !replaced {
			notif.Events = append(notif.Events, e)
		}
	}
	if req.Enabled != nil {
		notif.Enabled = *req.Enabled
	}
	newCfg.Notifications = &notif

	if err := newCfg.Validate(); err != nil {
		writeJSON(w, 400, map[string]string{"error": "validation: " + err.Error()})
		return
	}
	if err := s.deps.SaveConfig(s.deps.ConfigPath, &newCfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": "save failed: " + err.Error()})
		return
	}
	// Share the Settings apply path so runtime services see the saved config.
	configreload.Apply(s.deps.CfgMu, s.deps.Cfg, s.deps.CtrlMu, s.deps.Ctrl,
		&newCfg, s.deps.ConfigApplier)
	slog.Info("notification rules updated via API", "events", len(req.Events))
	writeNotificationRules(w, notif)
}

// callerDeviceID names the enrolled device behind a request, empty when the
// request came in off the LAN with no caller at all. Rows without a device
// are simply never swept by a revocation.
func callerDeviceID(r *http.Request) string {
	caller, ok := apiauth.FromRequest(r)
	if !ok || caller.Kind != apiauth.KindApp {
		return ""
	}
	return strings.TrimPrefix(caller.Subject, "app:")
}

func (s *Server) pokePushResync() {
	if s.deps.PushResync != nil {
		// Off this goroutine: the resync talks HTTP to the relay, and the
		// subscription route should answer at memory speed.
		go s.deps.PushResync()
	}
}
