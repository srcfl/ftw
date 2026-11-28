package api

import (
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/prices"
)

// priceAreaNote says when a box prices by an area nobody chose. An empty
// price.zone still runs on the SE3 fallback in prices.FromConfig, so a
// working site keeps its prices, but the owner can see why they may belong
// to another area. Empty when the area is set or prices are off.
func priceAreaNote(cfg *config.Price, svc *prices.Service) string {
	if cfg == nil || svc == nil || strings.TrimSpace(cfg.Zone) != "" {
		return ""
	}
	return "Price area not set, using " + svc.Zone
}
