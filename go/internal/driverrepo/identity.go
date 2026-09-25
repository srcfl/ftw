package driverrepo

import "strings"

// IdentifiesSameDriver reports whether a DRIVER table's declared id names the
// signed catalog driver catalogID. It is the publisher's own rule, the one the
// channel build enforces (srcfl/device-drivers tools/ftw_repository.py,
// _identifies_same_driver): source ids use hyphens and are often more
// specific, so the catalog id's words must appear in the declared id in
// order. "easee-cloud" names easee_cloud and "sungrow-shx" names sungrow; a
// growatt table in deye.lua does not.
//
// The rule is loose on purpose ("ctek-chargestorm-hybrid" also contains
// "ctek"), so callers pair it with the file the signed manifest names.
func IdentifiesSameDriver(declared, catalogID string) bool {
	if declared == "" || catalogID == "" {
		return false
	}
	words := func(value string) []string {
		return strings.Split(strings.ReplaceAll(value, "-", "_"), "_")
	}
	remaining := words(declared)
	next := 0
	for _, word := range words(catalogID) {
		for next < len(remaining) && remaining[next] != word {
			next++
		}
		if next == len(remaining) {
			return false
		}
		next++
	}
	return true
}
