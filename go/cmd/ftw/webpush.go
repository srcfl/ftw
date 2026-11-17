package main

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/notifications"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The web-push glue: state.Store adapted onto the narrow interfaces the
// notifications provider and the dead man's switch ask for. The adapters
// live here so neither package imports storage — the same seam the
// notification history already uses.

// pushSubscriptionSource is state.Store as the web-push provider sees it.
type pushSubscriptionSource struct{ st *state.Store }

func (p pushSubscriptionSource) PushSubscriptions() ([]notifications.PushSubscription, error) {
	rows, err := p.st.PushSubscriptions()
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

func (p pushSubscriptionSource) DeletePushSubscription(id string) error {
	return p.st.DeletePushSubscription(id)
}

// ownerByDevice says whether a paired phone is an owner now, read from the
// roster on each ask so a role changed a moment ago counts.
func ownerByDevice(enroll *appenroll.Identity) func(deviceID string) bool {
	return func(deviceID string) bool {
		for _, d := range enroll.Devices() {
			if d.ID == deviceID {
				return d.Role == apiauth.RoleOwner
			}
		}
		return false
	}
}

// webPushFromState renders and encrypts the box.unreachable farewell, one
// row per stored subscription. The sentence comes from the push catalogue
// and the encryption is the same RFC 8291 construction every live push
// uses — FTW Cloud stores it and can read none of it. Each row also carries
// the VAPID Authorization header the push service will demand, signed here
// by the provider's own key because the service must never hold that key; the
// roster calls back every six hours, so the signature is re-made long
// before its 24 hours run out.
type webPushFromState struct {
	st     *state.Store
	wp     *notifications.WebPush
	enroll *appenroll.Identity
}

func (d webPushFromState) WebPushRows() ([]cloudwire.WebPushRow, error) {
	subs, err := d.st.PushSubscriptions()
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, nil
	}
	title, body, err := notifications.RenderPush(notifications.PushBoxUnreachable, nil)
	if err != nil {
		return nil, fmt.Errorf("render box.unreachable: %w", err)
	}
	payload, err := json.Marshal(map[string]string{"title": title, "body": body})
	if err != nil {
		return nil, err
	}
	rows := make([]cloudwire.WebPushRow, 0, len(subs))
	for _, sub := range subs {
		device, ok := d.enroll.GrantFor(sub.DeviceID)
		if !ok {
			continue
		}
		ct, err := notifications.EncryptForSubscription(sub.P256dh, sub.Auth, payload)
		if err != nil {
			// One unencryptable subscription must not disarm the rest.
			slog.Warn("dead man's switch: skipping a subscription", "err", err)
			continue
		}
		auth, err := d.wp.DeadmanAuthorization(sub.Endpoint)
		if err != nil {
			slog.Warn("dead man's switch: skipping a subscription", "err", err)
			continue
		}
		row := cloudwire.WebPushRow{
			ID: sub.ID, Device: sub.DeviceID, Epoch: device.Epoch,
			Endpoint: sub.Endpoint,
			CT:       ct,
			Auth:     auth,
		}
		proposed := append(rows, row)
		if cloudwire.ValidateWebPush(proposed) != nil {
			slog.Warn("browser offline notice limit reached")
			break
		}
		rows = proposed
	}
	return rows, nil
}
