package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/srcfl/ftw/go/internal/api"
)

const minAPITokenLength = 32

// apiMutationPolicy keeps the legacy local-LAN workflow open while requiring
// an explicit secret for protected requests addressed through public/FQDN
// hostnames. An invalid configured token fails closed for remote requests.
func apiMutationPolicy() api.MutationPolicy {
	token := strings.TrimSpace(os.Getenv("FTW_API_TOKEN"))
	if token != "" && len(token) < minAPITokenLength {
		slog.Error("FTW_API_TOKEN is too short; remote API mutations remain disabled",
			"minimum_characters", minAPITokenLength)
		token = ""
	}
	return api.MutationPolicy{
		RequireTokenForRemote: true,
		Token:                 token,
	}
}
