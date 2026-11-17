package selfupdate

import (
	"fmt"
	"testing"
)

// A box that kept the single announced version before this change keeps
// it as the first of its list.
func TestAnnouncedVersionsReadTheOlderSingleValue(t *testing.T) {
	st := newMemStore()
	_ = st.SaveConfig(announcedKey, "v0.165.0-beta.1")
	got := announcedVersions(st)
	if len(got) != 1 || got[0] != "v0.165.0-beta.1" {
		t.Fatalf("announcedVersions = %v", got)
	}
	var list []string
	for i := range maxAnnounced + 5 {
		list = rememberAnnounced(list, fmt.Sprintf("v0.%d.0", 100+i))
	}
	if len(list) != maxAnnounced || list[len(list)-1] != fmt.Sprintf("v0.%d.0", 100+maxAnnounced+4) {
		t.Fatalf("kept %d versions ending %q, want the newest %d", len(list), list[len(list)-1], maxAnnounced)
	}
}
