package main

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/config"
)

// The config allowlist and the uplink's constants are the same two doors.
// A drift here would send commands to a host the other half does not dial.
func TestCommandDoorConstantsMatchTheUplink(t *testing.T) {
	if config.CommandRelayOrigin != appuplink.Endpoint {
		t.Fatalf("relay %q != uplink %q", config.CommandRelayOrigin, appuplink.Endpoint)
	}
	if config.CommandCloudOrigin != appuplink.CloudCommandOrigin {
		t.Fatalf("cloud %q != uplink %q", config.CommandCloudOrigin, appuplink.CloudCommandOrigin)
	}
}
