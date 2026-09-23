package components

import "os"

// KindHomeAssistantAddon is FTW_BUNDLE when Core runs as the Home Assistant app.
const KindHomeAssistantAddon = "home_assistant_addon"

// Bundle identifies a distribution that ships Core and its companion
// components in a single image whose host platform owns install, update and
// rollback (e.g. the Home Assistant add-on, where Supervisor replaces the
// whole bundle at once). In that packaging the per-component version
// breakdown is meaningless to the operator: everything moves together under
// one bundled FTW release.
type Bundle struct {
	// Kind names the packaging, e.g. "home_assistant_addon".
	Kind string `json:"kind"`
	// Version is the bundle's own release version (the add-on version),
	// distinct from the FTW Core version compiled into the binary.
	Version string `json:"version,omitempty"`
}

// ReexecOnRestart reports whether POST /api/restart must replace this
// process in place after a clean shutdown. The Home Assistant Supervisor
// only restarts a stopped app when Watchdog is on, and that toggle is off
// by default, so exiting would leave the app stopped
// (srcfl/home-assistant-addons#13). A nil Bundle is a native install and
// still exits so docker/systemd can bring the binary back.
func (b *Bundle) ReexecOnRestart() bool {
	return b != nil && b.Kind == KindHomeAssistantAddon
}

// BundleFromEnv reads FTW_BUNDLE and FTW_BUNDLE_VERSION, set by bundle
// packaging such as the Home Assistant add-on image. Nil means a native
// install where each component reports and updates independently.
func BundleFromEnv() *Bundle {
	kind := os.Getenv("FTW_BUNDLE")
	if kind == "" {
		return nil
	}
	return &Bundle{Kind: kind, Version: os.Getenv("FTW_BUNDLE_VERSION")}
}
