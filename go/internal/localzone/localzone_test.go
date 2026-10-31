package localzone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the binary embeds it too, so the zone names resolve as they do on a box
)

// host fakes the two files the resolver reads. Either may be left out.
func host(t *testing.T, linkedZone, timezoneFile string) (localtime, timezone string) {
	t.Helper()
	dir := t.TempDir()
	localtime = filepath.Join(dir, "localtime")
	timezone = filepath.Join(dir, "timezone")
	if linkedZone != "" {
		target := filepath.Join(dir, "usr", "share", "zoneinfo", filepath.FromSlash(linkedZone))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, localtime); err != nil {
			t.Fatal(err)
		}
	}
	if timezoneFile != "" {
		if err := os.WriteFile(timezone, []byte(timezoneFile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return localtime, timezone
}

// zone loads a zone the way Go loads time.Local.
func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestAHostWithoutTZIsNamedByItsLocaltimeLink(t *testing.T) {
	localtime, timezone := host(t, "Europe/Stockholm", "")
	if got := resolve("", zone(t, "Europe/Stockholm"), localtime, timezone, nil); got != "Europe/Stockholm" {
		t.Fatalf("zone = %q, want Europe/Stockholm", got)
	}
}

func TestTZNamesTheZoneBeforeTheHostFiles(t *testing.T) {
	localtime, timezone := host(t, "Europe/Stockholm", "Europe/Oslo\n")
	if got := resolve(":Europe/Berlin", zone(t, "Europe/Berlin"), localtime, timezone, nil); got != "Europe/Berlin" {
		t.Fatalf("zone = %q, want Europe/Berlin", got)
	}
}

// Go runs on UTC when it cannot load TZ, whatever the host files say.
func TestAnUnknownTZRunsOnUTC(t *testing.T) {
	localtime, timezone := host(t, "Europe/Stockholm", "")
	if got := resolve("Mars/Olympus", time.UTC, localtime, timezone, nil); got != "UTC" {
		t.Fatalf("zone = %q, want UTC", got)
	}
}

func TestEtcTimezoneNamesTheZoneWhenLocaltimeIsNotALink(t *testing.T) {
	localtime, timezone := host(t, "", "Europe/Oslo\n")
	if got := resolve("", zone(t, "Europe/Oslo"), localtime, timezone, nil); got != "Europe/Oslo" {
		t.Fatalf("zone = %q, want Europe/Oslo", got)
	}
}

func TestAHostThatNamesNoZoneIsUTC(t *testing.T) {
	localtime, timezone := host(t, "", "")
	if got := resolve("", time.UTC, localtime, timezone, nil); got != "UTC" {
		t.Fatalf("zone = %q, want UTC", got)
	}
}

// hostZoneFile is a real tzfile from this machine's zoneinfo tree.
func hostZoneFile(t *testing.T, name string) []byte {
	t.Helper()
	for _, root := range zoneinfoRoots {
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name))); err == nil {
			return b
		}
	}
	t.Skip("no zoneinfo tree on this host")
	return nil
}

// zoneTree fakes a zoneinfo tree: name → contents, or "->" and a link target.
func zoneTree(t *testing.T, files map[string]any) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := body.(string); ok {
			if err := os.Symlink(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(target, "->"))), path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, body.([]byte), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// copied is a host whose /etc/localtime is a copy of data, as a container
// gets with -v /etc/localtime:/etc/localtime:ro, beside the image's own
// /etc/timezone. It returns the zone Go runs on and the two paths.
func copied(t *testing.T, data []byte, timezoneFile string) (local *time.Location, localtime, timezone string) {
	t.Helper()
	localtime, timezone = host(t, "", timezoneFile)
	if err := os.WriteFile(localtime, data, 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := time.LoadLocationFromTZData("Local", data)
	if err != nil {
		t.Fatal(err)
	}
	return local, localtime, timezone
}

// A Debian container that mounts the host's localtime keeps the image's
// "Etc/UTC" in /etc/timezone. Go runs on the copied Stockholm rules; naming
// UTC put every car's ready time an hour late on the box's clients.
func TestACopiedLocaltimeIsNamedByItsZoneFileNotAStaleTimezone(t *testing.T) {
	stockholm := hostZoneFile(t, "Europe/Stockholm")
	root := zoneTree(t, map[string]any{
		"Arctic/Longyearbyen":    "->Europe/Stockholm",
		"Etc/UTC":                hostZoneFile(t, "Etc/UTC"),
		"Europe/Rome":            hostZoneFile(t, "Europe/Rome"),
		"Europe/Stockholm":       stockholm,
		"posix/Europe/Stockholm": stockholm,
	})
	local, localtime, timezone := copied(t, stockholm, "Etc/UTC\n")
	if got := resolve("", local, localtime, timezone, []string{root}); got != "Europe/Stockholm" {
		t.Fatalf("zone = %q, want Europe/Stockholm", got)
	}
}

// A file from another tzdata release differs in bytes; its rules still name it.
func TestACopiedLocaltimeIsNamedByItsRules(t *testing.T) {
	root := zoneTree(t, map[string]any{
		"Europe/Rome":      hostZoneFile(t, "Europe/Rome"),
		"Europe/Stockholm": hostZoneFile(t, "Europe/Stockholm"),
	})
	local, localtime, timezone := copied(t, hostZoneFile(t, "Europe/Berlin"), "")
	if got := resolve("", local, localtime, timezone, []string{filepath.Join(root, "missing"), root}); got != "Europe/Stockholm" {
		t.Fatalf("zone = %q, want Europe/Stockholm", got)
	}
}

func TestZonesWithOtherRulesDiffer(t *testing.T) {
	if sameRules(zone(t, "Europe/Stockholm"), zone(t, "Europe/London")) {
		t.Fatal("Stockholm and London have the same rules")
	}
	if sameRules(zone(t, "Europe/Stockholm"), time.UTC) {
		t.Fatal("Stockholm and UTC have the same rules")
	}
	if !sameRules(zone(t, "Etc/UTC"), time.UTC) {
		t.Fatal("Etc/UTC and UTC differ")
	}
}

func TestNameIsAZoneClientsCanLoad(t *testing.T) {
	got := Name()
	if got == "Local" || got == "" {
		t.Fatalf("Name() = %q, want an IANA name or UTC", got)
	}
	if _, err := time.LoadLocation(got); err != nil {
		t.Fatalf("Name() = %q does not load: %v", got, err)
	}
}

// Debian links /etc/localtime to Etc/UTC, so a container that mounts the
// host's localtime writes the host's rules over Etc/UTC itself. The link and
// the file then both say Etc/UTC; clients would read the box's clock as UTC.
func TestAMountOverTheEtcUTCLinkIsNotUTC(t *testing.T) {
	stockholm := hostZoneFile(t, "Europe/Stockholm")
	root := zoneTree(t, map[string]any{
		"Etc/UTC":          stockholm,
		"Europe/Rome":      hostZoneFile(t, "Europe/Rome"),
		"Europe/Stockholm": stockholm,
	})
	localtime, timezone := host(t, "", "")
	if err := os.Symlink(filepath.Join(root, "Etc", "UTC"), localtime); err != nil {
		t.Fatal(err)
	}
	local, err := time.LoadLocationFromTZData("Local", stockholm)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZONEINFO", root) // Go loads Etc/UTC from the overwritten tree, as in the container
	if got := resolve("", local, localtime, timezone, []string{root}); got != "Europe/Stockholm" {
		t.Fatalf("zone = %q, want Europe/Stockholm", got)
	}
}
