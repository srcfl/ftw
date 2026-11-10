package thermal

import "testing"

func TestValidAssetKind(t *testing.T) {
	for _, k := range []AssetKind{AssetSpaceHeat, AssetCooling, AssetDHW, AssetBuffer} {
		if !ValidAssetKind(k) {
			t.Errorf("ValidAssetKind(%q) = false", k)
		}
	}
	for _, k := range []AssetKind{"", "sauna", "Space_Heat"} {
		if ValidAssetKind(k) {
			t.Errorf("ValidAssetKind(%q) = true", k)
		}
	}
}
