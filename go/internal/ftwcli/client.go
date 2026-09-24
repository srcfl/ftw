package ftwcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type client struct {
	base string
	env  env
	http *http.Client
}

func newClient(base string, e env) *client {
	// No client-wide timeout: each call sets its own, and copying a large
	// backup must not be cut off by one.
	return &client{base: base, env: e, http: &http.Client{}}
}

// apiError is a non-2xx answer from Core.
type apiError struct {
	method, path string
	status       int
	message      string
	body         []byte
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.method, e.path, e.status, e.message)
}

func (c *client) get(ctx context.Context, path string, dest any) error {
	return c.call(ctx, http.MethodGet, path, nil, dest, c.env.requestTimeout)
}

func (c *client) post(ctx context.Context, path string, dest any) error {
	return c.call(ctx, http.MethodPost, path, map[string]any{}, dest, c.env.requestTimeout)
}

// call sends one request. A zero timeout leaves only ctx to bound it.
func (c *client) call(ctx context.Context, method, path string, body, dest any, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{method: method, path: path, status: resp.StatusCode, message: errorMessage(payload), body: payload}
	}
	if dest == nil || len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, dest)
}

func errorMessage(payload []byte) string {
	var body struct {
		Error string `json:"error"`
		Err   string `json:"err"`
	}
	if json.Unmarshal(payload, &body) == nil {
		if body.Error != "" {
			return body.Error
		}
		if body.Err != "" {
			return body.Err
		}
	}
	if text := strings.TrimSpace(string(payload)); text != "" {
		return text
	}
	return "no reason given"
}

// open starts a download of a large body; only ctx bounds it.
func (c *client) open(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &apiError{method: http.MethodGet, path: path, status: resp.StatusCode, message: errorMessage(payload)}
	}
	return resp, nil
}
