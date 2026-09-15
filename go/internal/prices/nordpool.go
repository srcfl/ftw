package prices

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NordPoolProvider reads the public Nord Pool day-ahead dataportal.
// No API key. Used when the Sourceful harvest has not yet stored a day
// that Nord Pool has already published.
type NordPoolProvider struct {
	Client   *http.Client
	BaseURL  string
	Currency string
	FX       FXConverter
}

const nordPoolDefaultURL = "https://dataportal-api.nordpoolgroup.com/api/DayAheadPrices"

func NewNordPool() *NordPoolProvider {
	return &NordPoolProvider{
		Client:  &http.Client{Timeout: 15 * time.Second},
		BaseURL: nordPoolDefaultURL,
	}
}

func (n *NordPoolProvider) Name() string { return "nordpool" }

func (n *NordPoolProvider) apiCurrency() string {
	want := strings.ToUpper(strings.TrimSpace(n.Currency))
	switch want {
	case "SEK", "EUR", "NOK", "DKK", "GBP":
		return want
	default:
		if want == "" {
			return "SEK"
		}
		return "EUR"
	}
}

func (n *NordPoolProvider) Fetch(ctx context.Context, zone string, day time.Time) ([]RawPrice, error) {
	zone = strings.ToUpper(strings.TrimSpace(zone))
	if zone == "" {
		zone = "SE3"
	}
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		loc = time.UTC
	}
	date := day.In(loc).Format("2006-01-02")
	apiCur := n.apiCurrency()
	base := n.BaseURL
	if base == "" {
		base = nordPoolDefaultURL
	}
	endpoint := fmt.Sprintf("%s?date=%s&market=DayAhead&deliveryArea=%s&currency=%s",
		base, url.QueryEscape(date), url.QueryEscape(zone), url.QueryEscape(apiCur))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ftw (https://github.com/srcfl/ftw)")
	client := n.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("nordpool: status %d: %s", resp.StatusCode, string(body))
	}
	var payload struct {
		Currency         string `json:"currency"`
		MultiAreaEntries []struct {
			DeliveryStart string             `json:"deliveryStart"`
			DeliveryEnd   string             `json:"deliveryEnd"`
			EntryPerArea  map[string]float64 `json:"entryPerArea"`
		} `json:"multiAreaEntries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("nordpool: decode: %w", err)
	}
	if payload.Currency != "" && !strings.EqualFold(payload.Currency, apiCur) {
		return nil, fmt.Errorf("nordpool: asked for %s, got %q", apiCur, payload.Currency)
	}
	want := strings.ToUpper(strings.TrimSpace(n.Currency))
	if want == "" {
		want = "SEK"
	}
	convert := want != apiCur
	if convert && n.FX == nil {
		return nil, fmt.Errorf("nordpool: no exchange rate source for %s→%s", apiCur, want)
	}
	out := make([]RawPrice, 0, len(payload.MultiAreaEntries))
	for _, e := range payload.MultiAreaEntries {
		price, ok := e.EntryPerArea[zone]
		if !ok {
			for k, v := range e.EntryPerArea {
				if strings.EqualFold(k, zone) {
					price, ok = v, true
					break
				}
			}
		}
		if !ok {
			continue
		}
		start, err := time.Parse(time.RFC3339, e.DeliveryStart)
		if err != nil {
			return nil, fmt.Errorf("nordpool: deliveryStart %q: %w", e.DeliveryStart, err)
		}
		slotMin := 15
		if e.DeliveryEnd != "" {
			if end, err := time.Parse(time.RFC3339, e.DeliveryEnd); err == nil {
				d := int(end.Sub(start).Minutes())
				if d >= 5 && d <= 120 {
					slotMin = d
				}
			}
		}
		perKWh := price / 1000.0
		if convert {
			native, ok := n.FX.Convert(perKWh, apiCur, want)
			if !ok {
				return nil, fmt.Errorf("nordpool: no %s→%s rate yet", apiCur, want)
			}
			perKWh = native
		}
		out = append(out, RawPrice{SlotStart: start, SlotLenMin: slotMin, SEKPerKWh: perKWh})
	}
	return out, nil
}
