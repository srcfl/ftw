package selfupdate

import (
	"context"
	"testing"
)

// A box installed from a beta tag that never chose a channel must keep
// following beta after it installs the stable that beta promotes to.
func TestNew_BetaBoxStaysOnBetaAfterStablePromotion(t *testing.T) {
	st := newMemStore() // same state.db across the restart

	// Boot 1: running a beta, no saved channel. Beta offers the promoted stable.
	rls := fakeReleasesServer(t, nil, fakeRelease{tag: "v0.158.0"})
	c := newCheckerOnFakes(t, "v0.158.0-beta.3", rls, st)
	info, err := c.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	t.Logf("running v0.158.0-beta.3: channel=%s latest=%s update_available=%v", info.Channel, info.Latest, info.UpdateAvailable)
	if info.Channel != ChannelBeta || info.Latest != "v0.158.0" || !info.UpdateAvailable {
		t.Fatalf("beta box should be offered the promoted stable: %+v", info)
	}

	// Boot 2: the stable is installed; a new beta is published.
	rls2 := fakeReleasesServer(t, nil,
		fakeRelease{tag: "v0.159.0-beta.1", prerelease: true},
		fakeRelease{tag: "v0.158.0"},
	)
	c2 := newCheckerOnFakes(t, "v0.158.0", rls2, st)
	info, err = c2.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	t.Logf("running v0.158.0: channel=%s latest=%s update_available=%v", info.Channel, info.Latest, info.UpdateAvailable)
	if info.Channel != ChannelBeta {
		t.Fatalf("channel flipped to %q only because the version string changed", info.Channel)
	}
	if info.Latest != "v0.159.0-beta.1" || !info.UpdateAvailable {
		t.Fatalf("next beta not offered: %+v", info)
	}
}

// A build without a release tag implies no channel, so it saves none.
func TestNew_DevBuildSavesNoChannel(t *testing.T) {
	st := newMemStore()
	New(Config{CurrentVersion: "dev"}, st)
	if got, ok := st.LoadConfig(channelKey); ok {
		t.Fatalf("dev build saved channel %q", got)
	}
}
