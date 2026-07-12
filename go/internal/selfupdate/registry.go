package selfupdate

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

// registryProbe asks an OCI registry which immutable version and edge tags
// exist for an image. Listing them proves a resolved target is deployable
// without installing moving aliases such as :latest, :beta, or :edge.
type registryProbe struct {
	httpClient *http.Client
	// base is the registry root, e.g. "https://ghcr.io". Overridable for tests.
	base string
	// repo is the registry path, e.g. "frahlg/forty-two-watts".
	repo string
	// service is the audience for the token request. ghcr.io expects "ghcr.io".
	service string
}

// hasTag reports whether the registry currently has the given tag in
// its /tags/list. Tags appear there only when their manifest has been
// pushed, so a true answer is proof the image is deployable.
func (rp *registryProbe) hasTag(ctx context.Context, tag string) (bool, error) {
	tok, err := rp.token(ctx)
	if err != nil {
		return false, err
	}
	tags, err := rp.listTags(ctx, tok)
	if err != nil {
		return false, err
	}
	for _, t := range tags {
		if t == tag {
			return true, nil
		}
	}
	return false, nil
}

// latestEdgeTag returns the newest immutable edge tag. Edge publishers use
// edge-YYYYMMDDTHHMMSSZ-<shortsha>, so lexical order is chronological and a
// moving :edge alias never becomes an install target.
func (rp *registryProbe) latestEdgeTag(ctx context.Context) (string, error) {
	tok, err := rp.token(ctx)
	if err != nil {
		return "", err
	}
	tags, err := rp.listTags(ctx, tok)
	if err != nil {
		return "", err
	}
	latest := ""
	for _, tag := range tags {
		if validEdgeTag(tag) && tag > latest {
			latest = tag
		}
	}
	return latest, nil
}

func validEdgeTag(tag string) bool {
	parts := strings.Split(tag, "-")
	if len(parts) != 3 || parts[0] != "edge" || len(parts[2]) < 7 {
		return false
	}
	if _, err := time.Parse("20060102T150405Z", parts[1]); err != nil {
		return false
	}
	for _, r := range parts[2] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func (rp *registryProbe) token(ctx context.Context) (string, error) {
	u := rp.base + "/token?service=" + url.QueryEscape(rp.service) +
		"&scope=" + url.QueryEscape("repository:"+rp.repo+":pull")
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := rp.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("registry token: HTTP %d", resp.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", err
	}
	if t.Token != "" {
		return t.Token, nil
	}
	return t.AccessToken, nil
}

func (rp *registryProbe) listTags(ctx context.Context, token string) ([]string, error) {
	var all []string
	next := rp.base + "/v2/" + rp.repo + "/tags/list?n=200"
	for next != "" {
		req, _ := http.NewRequestWithContext(ctx, "GET", next, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := rp.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			return nil, fmt.Errorf("tags/list: HTTP %d", resp.StatusCode)
		}
		var page struct {
			Tags []string `json:"tags"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, err
		}
		all = append(all, page.Tags...)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		next = parseNextLink(link, rp.base)
	}
	// An empty list is intentionally not an error — that's the race
	// window between GH publishing the release and the build workflow
	// pushing the image. Caller (hasTag) returns false, Check reports
	// update_available=false this cycle and re-probes next tick.
	return all, nil
}

// parseNextLink extracts the rfc5988 `rel="next"` URL from a Link header
// (used by OCI distribution registries to paginate /tags/list). Relative
// URLs are resolved against base.
func parseNextLink(header, base string) string {
	if header == "" {
		return ""
	}
	for _, p := range strings.Split(header, ",") {
		p = strings.TrimSpace(p)
		if !strings.Contains(p, `rel="next"`) {
			continue
		}
		i := strings.Index(p, "<")
		j := strings.Index(p, ">")
		if i < 0 || j < 0 || j <= i {
			continue
		}
		u := p[i+1 : j]
		if strings.HasPrefix(u, "/") {
			u = base + u
		}
		return u
	}
	return ""
}
