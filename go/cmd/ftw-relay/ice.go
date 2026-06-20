package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const turnCredentialTTL = 12 * time.Hour

type iceServerWire struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
	TTL        int      `json:"ttl,omitempty"`
}

type signalICEWire struct {
	ICEServers []iceServerWire `json:"ice_servers"`
}

func parseURLList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// signalICE publishes the ICE config browsers and Pis should use for the
// signed WebRTC channel. TURN credentials follow coturn's REST API scheme:
// username is an expiry timestamp and credential is HMAC-SHA1(secret, username).
func (r *Relay) signalICE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")

	var out signalICEWire
	if len(r.ICEStunURLs) > 0 {
		out.ICEServers = append(out.ICEServers, iceServerWire{URLs: append([]string(nil), r.ICEStunURLs...)})
	}
	if len(r.TURNURLs) > 0 && r.TURNSecret != "" {
		username := strconv.FormatInt(time.Now().Add(turnCredentialTTL).Unix(), 10)
		mac := hmac.New(sha1.New, []byte(r.TURNSecret))
		_, _ = mac.Write([]byte(username))
		out.ICEServers = append(out.ICEServers, iceServerWire{
			URLs:       append([]string(nil), r.TURNURLs...),
			Username:   username,
			Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
			TTL:        int(turnCredentialTTL.Seconds()),
		})
	}
	_ = json.NewEncoder(w).Encode(out)
}
