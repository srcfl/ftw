package fleetping

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	usageFreshFor   = 6 * time.Hour
	usageRetryAfter = 10 * time.Minute
	usageTimeout    = 2 * time.Second
	usageMaxBytes   = 2 << 20
)

// Usage reads how many boxes ran each driver version on the last closed day,
// from the fleet's public counts. It is asked only when someone opens a list
// of drivers, carries nothing about this box, and an answer that does not come
// only leaves the number off the screen.
type Usage struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu     sync.Mutex
	counts map[string]int
	nextAt time.Time
}

// NewUsage reads the public counts beside a fleet report endpoint.
func NewUsage(endpoint string) *Usage {
	return &Usage{
		url:    strings.TrimRight(endpoint, "/") + "/public",
		client: &http.Client{Timeout: usageTimeout},
		now:    time.Now,
	}
}

// Counts is the last closed day's reports per "driver@version", or nil when
// they are not known. A reply is kept for hours and a failure for minutes, so
// a list opens at once however often it is opened.
func (u *Usage) Counts(ctx context.Context) map[string]int {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	if now.Before(u.nextAt) {
		return u.counts
	}
	counts, err := u.fetch(ctx)
	if err != nil {
		u.nextAt = now.Add(usageRetryAfter)
		return u.counts
	}
	u.counts, u.nextAt = counts, now.Add(usageFreshFor)
	return u.counts
}

func (u *Usage) fetch(ctx context.Context) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url, nil)
	if err != nil {
		return nil, err
	}
	res, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, io.ErrUnexpectedEOF
	}
	var feed struct {
		Days []struct {
			Date    string         `json:"date"`
			Drivers map[string]int `json:"drivers"`
		} `json:"days"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, usageMaxBytes)).Decode(&feed); err != nil {
		return nil, err
	}
	newest, counts := "", map[string]int{}
	for _, d := range feed.Days {
		if d.Date > newest {
			newest, counts = d.Date, d.Drivers
		}
	}
	return counts, nil
}
