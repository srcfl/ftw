package components

import "testing"

func TestBundleFromEnvUnsetMeansNativeInstall(t *testing.T) {
	t.Setenv("FTW_BUNDLE", "")
	t.Setenv("FTW_BUNDLE_VERSION", "0.1.0-beta.1")
	if got := BundleFromEnv(); got != nil {
		t.Fatalf("BundleFromEnv() = %+v, want nil without FTW_BUNDLE", got)
	}
}

func TestBundleFromEnvReadsKindAndVersion(t *testing.T) {
	t.Setenv("FTW_BUNDLE", KindHomeAssistantAddon)
	t.Setenv("FTW_BUNDLE_VERSION", "0.1.0-beta.1")
	got := BundleFromEnv()
	if got == nil || got.Kind != KindHomeAssistantAddon || got.Version != "0.1.0-beta.1" {
		t.Fatalf("BundleFromEnv() = %+v, want home_assistant_addon 0.1.0-beta.1", got)
	}
}

func TestReexecOnRestart(t *testing.T) {
	var unset *Bundle
	if unset.ReexecOnRestart() {
		t.Fatal("nil bundle must not re-exec; native installs exit so docker/systemd can restart")
	}
	if (&Bundle{Kind: "compose"}).ReexecOnRestart() {
		t.Fatal("unknown bundle kinds must not re-exec")
	}
	if !(&Bundle{Kind: KindHomeAssistantAddon}).ReexecOnRestart() {
		t.Fatal("Home Assistant add-on must re-exec; Supervisor will not restart a stopped app")
	}
}
