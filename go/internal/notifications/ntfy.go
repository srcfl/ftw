package notifications

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// ntfyProvider publishes to ntfy.sh (or a self-hosted ntfy server).
// Registered under the "ntfy" name at package init via the strategy
// registry in service.go.
type ntfyProvider struct {
	mu   sync.Mutex
	cfg  *config.NtfyConfig
	http *http.Client
}

func init() {
	RegisterProvider("ntfy", newNtfyProvider)
}

func newNtfyProvider(cfg *config.Notifications) Provider {
	p := &ntfyProvider{http: &http.Client{Timeout: 10 * time.Second}}
	p.SetConfig(cfg)
	return p
}

// Name identifies this provider in status responses and logs.
func (p *ntfyProvider) Name() string { return "ntfy" }

// SetConfig hot-swaps the transport settings without touching in-flight requests.
func (p *ntfyProvider) SetConfig(cfg *config.Notifications) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cfg != nil && cfg.Ntfy != nil {
		c := *cfg.Ntfy
		p.cfg = &c
	} else {
		p.cfg = nil
	}
}

// Publish POSTs the message to ntfy. Returns an error on missing config,
// transport failure, or non-2xx response.
func (p *ntfyProvider) Publish(ctx context.Context, m Message) error {
	p.mu.Lock()
	cfg := p.cfg
	httpc := p.http
	p.mu.Unlock()
	if cfg == nil {
		return fmt.Errorf("ntfy: not configured")
	}
	if strings.TrimSpace(cfg.Server) == "" {
		return fmt.Errorf("ntfy: server not set")
	}
	if strings.TrimSpace(cfg.Topic) == "" {
		return fmt.Errorf("ntfy: topic not set")
	}
	server := strings.TrimRight(cfg.Server, "/")
	reqURL := server + "/" + url.PathEscape(cfg.Topic)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(m.Body))
	if err != nil {
		return fmt.Errorf("ntfy: build request: %w", err)
	}
	if m.Title != "" {
		req.Header.Set("Title", m.Title)
	}
	if m.Priority > 0 {
		req.Header.Set("Priority", fmt.Sprintf("%d", m.Priority))
	}
	if len(m.Tags) > 0 {
		req.Header.Set("Tags", strings.Join(m.Tags, ","))
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	// Auth precedence: bearer > basic > none.
	if strings.TrimSpace(cfg.AccessToken) != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AccessToken)
	} else if cfg.Username != "" || cfg.Password != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(cfg.Username + ":" + cfg.Password))
		req.Header.Set("Authorization", "Basic "+auth)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ntfy: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
