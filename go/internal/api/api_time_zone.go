package api

import (
	"errors"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/configreload"
	"github.com/srcfl/ftw/go/internal/localzone"
)

// householdZoneKnown is localzone.Known; tests stand in a host on UTC.
var householdZoneKnown = localzone.Known

// ProposeTimeZone stores a client's time zone as the site's household clock,
// when the box keeps none: no zone stored, and a host on UTC. A box that
// keeps one answers with it, so a phone abroad never moves the house's clock;
// the owner changes a kept zone in setup.
//
// Core sets the clock once at start, so a zone with other rules than the
// running clock restarts Core. That happens once per box, on its first
// offer.
func (s *Server) ProposeTimeZone(zone string) (appproto.TimeZoneAnswer, error) {
	loc, err := localzone.Load(zone)
	if err != nil {
		return appproto.TimeZoneAnswer{}, err
	}
	if s.deps.Cfg == nil || s.deps.CfgMu == nil || s.deps.SaveConfig == nil {
		return appproto.TimeZoneAnswer{}, errors.New("settings are not available yet")
	}
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()

	s.deps.CfgMu.RLock()
	next := *s.deps.Cfg
	s.deps.CfgMu.RUnlock()
	if next.Site.TimeZone != "" {
		return appproto.TimeZoneAnswer{Zone: next.Site.TimeZone}, nil
	}
	if householdZoneKnown() {
		return appproto.TimeZoneAnswer{Zone: localzone.Name()}, nil
	}
	next.Site.TimeZone = zone
	if err := s.deps.SaveConfig(s.deps.ConfigPath, &next); err != nil {
		return appproto.TimeZoneAnswer{}, err
	}
	configreload.Apply(s.deps.CfgMu, s.deps.Cfg, s.deps.CtrlMu, s.deps.Ctrl, &next, s.deps.ConfigApplier)
	restart := !localzone.SameRules(loc, time.Local)
	slog.Info("household time zone stored from a client", "tz", zone, "restart", restart)
	if restart && s.deps.Restart != nil {
		s.queueRestart(nil, nil)
	}
	return appproto.TimeZoneAnswer{Zone: zone, Taken: true}, nil
}
