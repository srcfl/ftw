// Package localzone keeps the household's clock: the zone every wall-clock
// rule reads, and its name for clients.
package localzone

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Name returns the IANA name of the box's zone, or "UTC".
//
// Go names the host zone "Local" when TZ is unset, and no client can load a
// zone by that name. Clients render plan times and days in this zone, and
// persisted clock buckets keep its DST rules, so it must be a name that
// time.LoadLocation and Intl accept. UTC is valid when that is the box's
// configured zone; never guess a country.
//
// The name must keep the rules time.Local runs on. Clients read the box's
// household clock (a car's ready time, a goal's days) in this zone, so a name
// with other rules moves every goal by the difference.
//
// A zone the site stores (Use) wins over the host's.
func Name() string {
	if household != "" {
		return household
	}
	return name()
}

// household is the zone the site stores, set once by Use.
var household string

// Stored returns the zone the site stores, or "" when the box runs on the
// host's zone.
func Stored() string { return household }

// Known reports whether the box has a household clock: a zone the site
// stores, or a host zone other than UTC. A Docker box has none until a
// client offers one.
func Known() bool {
	return household != "" || !utcNames[strings.TrimPrefix(name(), "Etc/")]
}

// Use makes the site's stored zone the household clock: time.Local, and the
// Name clients read. A Docker box runs on UTC whatever house it is in, so a
// car goal of 17:00 was met at 18:00 in a Stockholm winter until the zone
// became a fact of the site rather than of the host.
//
// Core calls it once at start, before any goroutine reads the clock:
// assigning time.Local later races every time.Now. A change takes effect
// when Core next starts. Empty keeps the host's zone and names it again.
func Use(zone string) error {
	if zone == "" {
		household = ""
		return nil
	}
	loc, err := Load(zone)
	if err != nil {
		return err
	}
	time.Local = loc
	household = zone
	// A worker Core starts reads the same clock.
	return os.Setenv("TZ", zone)
}

// Load reads an IANA zone a site may store. "Local" and "" name no zone.
func Load(zone string) (*time.Location, error) {
	if zone == "" || zone == "Local" {
		return nil, errors.New("name a time zone such as Europe/Stockholm")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("%q is not a time zone such as Europe/Stockholm", zone)
	}
	return loc, nil
}

// SameRules reports whether a and b keep the same clock from 1970 to 2038.
func SameRules(a, b *time.Location) bool { return sameRules(a, b) }

// Read once: time.Local is fixed when the process starts, so a zone changed
// on the host afterwards must not rename the one the box still runs on.
var name = sync.OnceValue(func() string {
	return resolve(os.Getenv("TZ"), time.Local, "/etc/localtime", "/etc/timezone", zoneinfoRoots)
})

// zoneinfoRoots are where hosts keep the zone files a copied /etc/localtime
// comes from.
var zoneinfoRoots = []string{"/usr/share/zoneinfo", "/usr/lib/zoneinfo", "/usr/share/lib/zoneinfo"}

func resolve(tz string, local *time.Location, localtime, timezone string, roots []string) string {
	candidates := []string{strings.TrimPrefix(tz, ":"), local.String()}
	if target, err := filepath.EvalSymlinks(localtime); err == nil {
		if _, zone, ok := strings.Cut(target, "/zoneinfo/"); ok {
			candidates = append(candidates, zone)
		}
	}
	// Debian keeps /etc/timezone beside /etc/localtime, and a container
	// that mounts only the latter keeps the image's stale name.
	if data, err := os.ReadFile(timezone); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(data)))
	}
	for _, zone := range candidates {
		if zone == "" || zone == "Local" {
			continue
		}
		if loc, err := time.LoadLocation(zone); err == nil && sameRules(loc, local) && meansWhatItSays(zone, local) {
			return zone
		}
	}
	// A copied file, such as a bind-mounted /etc/localtime, carries the
	// rules Go runs on but no name.
	data, _ := os.ReadFile(localtime)
	if zone := matchingZone(local, data, roots); zone != "" {
		return zone
	}
	return "UTC"
}

// matchingZone names a zone file under roots with local's rules: one with
// exactly these bytes if there is one, else the first in lexical order. Links
// and the posix/right variants repeat a zone under other names, so only
// regular Area/City files count.
func matchingZone(local *time.Location, data []byte, roots []string) string {
	first := ""
	for _, root := range roots {
		exact := ""
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel == "posix" || rel == "right" {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || !strings.Contains(rel, "/") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			loc, err := time.LoadLocationFromTZData(rel, b)
			if err != nil || !sameRules(loc, local) || !meansWhatItSays(rel, local) {
				return nil
			}
			if _, err := time.LoadLocation(rel); err != nil {
				return nil
			}
			if bytes.Equal(b, data) {
				exact = rel
				return filepath.SkipAll
			}
			if first == "" {
				first = rel
			}
			return nil
		})
		if exact != "" {
			return exact
		}
	}
	return first
}

// utcNames are the names clients read as UTC.
var utcNames = map[string]bool{
	"UTC": true, "UCT": true, "Universal": true, "Zulu": true,
	"GMT": true, "GMT0": true, "GMT+0": true, "GMT-0": true, "Greenwich": true,
}

// meansWhatItSays rejects a UTC name for a clock that is not UTC. Debian
// links /etc/localtime to Etc/UTC, so mounting the host's file over it
// overwrites Etc/UTC itself: that name then loads the host's rules here,
// and UTC on every client.
func meansWhatItSays(name string, local *time.Location) bool {
	return !utcNames[strings.TrimPrefix(name, "Etc/")] || sameRules(time.UTC, local)
}

// sameRules reports whether a and b show the same UTC offset at every
// instant from 1970 to 2038. Older history differs between tzdata releases
// and moves no clock the box keeps.
func sameRules(a, b *time.Location) bool {
	return follows(a, b) && follows(b, a)
}

// follows reports whether b shows a's offset at the start of each of a's
// periods. Run both ways, that covers every change in either zone.
func follows(a, b *time.Location) bool {
	end := time.Date(2038, 1, 1, 0, 0, 0, 0, time.UTC)
	for t := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC); t.Before(end); {
		in := t.In(a)
		_, want := in.Zone()
		if _, got := t.In(b).Zone(); got != want {
			return false
		}
		_, next := in.ZoneBounds()
		if !next.After(t) {
			return true
		}
		t = next
	}
	return true
}
