package drivers

import (
	net_http "net/http"
	net_url "net/url"

	"github.com/srcfl/ftw/go/internal/mdnsresolve"
)

// guardMDNSProxy checks the original request destination before delegating to
// proxy selection. HTTP transports and WebSocket dialers call their proxy
// function before NetDialContext, so a check in the dialer alone is too late:
// a denied .local request could already have reached a proxy with credentials
// or a command payload.
func guardMDNSProxy(proxy func(*net_http.Request) (*net_url.URL, error), allowUnverifiedLocal bool) func(*net_http.Request) (*net_url.URL, error) {
	return func(req *net_http.Request) (*net_url.URL, error) {
		if req != nil && req.URL != nil {
			if err := mdnsresolve.CheckLocalDestination(req.URL.Hostname(), allowUnverifiedLocal); err != nil {
				return nil, err
			}
		}
		if proxy == nil {
			return nil, nil
		}
		return proxy(req)
	}
}
