package drivers

import (
	"strings"
	"testing"
)

// A read-only driver that reads a vendor cloud cannot read anything until it
// has exchanged a token, and it exchanges one from init or poll. Those are the
// phases allowWrite refuses, so before this the choice was to publish such a
// driver control-capable -- which is what myuplink did, and why its catalog
// entry claimed a control path its driver_command has always refused.

func readOnlyAuthPostPolicy(path string) *RuntimePolicy {
	return &RuntimePolicy{
		PackageID:      "com.sourceful.driver.myuplink",
		Version:        "1.2.0",
		ArtifactSHA256: strings.Repeat("a", 64),
		ReadOnly:       true,
		Permissions:    map[string]bool{"http.get": true, "http.post": true},
		AuthPostPath:   path,
	}
}

func TestReadOnlyDriverMaySignInOutsideAWriteScope(t *testing.T) {
	env := &HostEnv{RuntimePolicy: readOnlyAuthPostPolicy("/oauth/token")}

	// No write scope is open -- this is what init and poll look like.
	if err := env.allowWrite("http.post"); err == nil {
		t.Fatal("allowWrite should still refuse a POST outside a write scope")
	}
	if !env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Fatal("the declared sign-in must be allowed from init or poll")
	}
	// A token refresh is driven by expiry, not by a caller, so it must not
	// spend the write budget that a real command depends on.
	if env.writeAttempts != 0 {
		t.Fatalf("sign-in consumed the write budget: %d", env.writeAttempts)
	}
}

func TestSignInExemptionIsConfinedToTheDeclaredPath(t *testing.T) {
	env := &HostEnv{RuntimePolicy: readOnlyAuthPostPolicy("/oauth/token")}

	for _, url := range []string{
		"https://api.myuplink.com/v2/devices/1/points",   // the write it must not do
		"https://api.myuplink.com/oauth/token/../device", // no path games
		"https://api.myuplink.com/oauth",
		"https://evil.example/oauth/token/extra",
		"not a url at all",
	} {
		if env.allowAuthPost(url) {
			t.Errorf("POST to %q must not pass as authentication", url)
		}
	}

	// A query string is part of the request, not the path.
	if !env.allowAuthPost("https://api.myuplink.com/oauth/token?x=1") {
		t.Error("a query string must not defeat the declared path")
	}
}

func TestSignInExemptionRequiresBeingDeclared(t *testing.T) {
	// Undeclared: the ordinary write rules apply, which is every driver today.
	env := &HostEnv{RuntimePolicy: readOnlyAuthPostPolicy("")}
	if env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Error("a driver that declared nothing must not be exempt")
	}

	// Declared but not read-only: a controlling driver has no exemption to
	// take, and must not gain a POST path that skips the write scope.
	control := readOnlyAuthPostPolicy("/oauth/token")
	control.ReadOnly = false
	env = &HostEnv{RuntimePolicy: control}
	if env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Error("a controlling driver must not use the read-only exemption")
	}

	// Declared, read-only, but the permission was never granted.
	missing := readOnlyAuthPostPolicy("/oauth/token")
	missing.Permissions = map[string]bool{"http.get": true}
	env = &HostEnv{RuntimePolicy: missing}
	if env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Error("the exemption must still require http.post to be granted")
	}

	// An unmanaged driver has no policy and is unaffected either way.
	env = &HostEnv{}
	if env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Error("a driver with no runtime policy must not report an exemption")
	}
}
