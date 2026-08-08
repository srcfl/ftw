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
	t.Setenv("FTW_BUNDLE", "home_assistant_addon")
	t.Setenv("FTW_BUNDLE_VERSION", "0.1.0-beta.1")
	got := BundleFromEnv()
	if got == nil || got.Kind != "home_assistant_addon" || got.Version != "0.1.0-beta.1" {
		t.Fatalf("BundleFromEnv() = %+v, want home_assistant_addon 0.1.0-beta.1", got)
	}
}
