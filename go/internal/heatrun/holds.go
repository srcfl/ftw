package heatrun

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/srcfl/ftw/go/internal/drivercontrol"
)

// HoldsWriter writes through the same holds an operator's control request
// uses, so the two share one lock and one expiry and neither can expire into
// a default that undoes the other.
type HoldsWriter struct{ Holds *drivercontrol.Holds }

// Hold sends the value the way POST /api/drivers/{name}/control does.
func (w HoldsWriter) Hold(ctx context.Context, driver, control string, value float64, d time.Duration) (int64, error) {
	body, err := json.Marshal(map[string]any{"action": control, "value": value, "offset": value})
	if err != nil {
		return 0, err
	}
	var (
		exp  int64
		sent error
	)
	w.Holds.Send(ctx, driver, control, value, body, d, func(h drivercontrol.Hold, err error) {
		exp, sent = h.ExpiresAt, err
	})
	if sent == nil && exp == 0 {
		return 0, errors.New("the hold was not armed")
	}
	return exp, sent
}

// Active is the driver's hold now, if any.
func (w HoldsWriter) Active(driver string) (string, int64, bool) {
	h := w.Holds.Active(driver)
	if h == nil {
		return "", 0, false
	}
	return h.Control, h.ExpiresAt, true
}

// Release ends the hold and sends the driver's own default.
func (w HoldsWriter) Release(ctx context.Context, driver string) error {
	return w.Holds.SendDefault(ctx, driver)
}
