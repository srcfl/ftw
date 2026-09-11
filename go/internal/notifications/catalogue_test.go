package notifications

import (
	"os"
	"testing"

	"github.com/srcfl/ftw/go/internal/appproto/gencontract"
)

// The sentence table must be what the catalogue currently says. A snapshot
// updated without rerunning the generator is exactly the drift the pairing
// exists to prevent — these are the only words the box may put on a lock
// screen, and they are the app's words.
func TestPushSentencesAreCurrent(t *testing.T) {
	raw, err := os.ReadFile("../../../contract/push-catalogue.yaml")
	if err != nil {
		t.Fatalf("read push catalogue: %v", err)
	}
	want, err := gencontract.GeneratePush(raw)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile("catalogue_gen.go")
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("notifications/catalogue_gen.go is stale; run: go generate ./internal/...")
	}
}

func TestRenderPushFillsPlaceholders(t *testing.T) {
	title, body, err := RenderPush(PushChargingSessionComplete, map[string]string{"kwh": "7.4"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if title != "Car charged" {
		t.Fatalf("title = %q", title)
	}
	if body != "7.4 kWh delivered — ready to go." {
		t.Fatalf("body = %q", body)
	}

	title, body, err = RenderPush(PushUpdateInstalled, map[string]string{"version": "v1.17.0"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if body != "Now running v1.17.0. Everything came back on its own." {
		t.Fatalf("body = %q", body)
	}
	if title != "Your box updated itself" {
		t.Fatalf("title = %q", title)
	}

	title, body, err = RenderPush(PushDriverOffline, map[string]string{"name": "Batteri"})
	if err != nil {
		t.Fatalf("render driver.offline: %v", err)
	}
	if title != "A device went quiet" || body != "Batteri stopped answering." {
		t.Fatalf("driver.offline = %q / %q", title, body)
	}

	title, body, err = RenderPush(PushFuseOverLimit, map[string]string{"phase": "L1"})
	if err != nil {
		t.Fatalf("render fuse.over_limit: %v", err)
	}
	if title != "The house is drawing too much" || body != "L1 is over the fuse rating." {
		t.Fatalf("fuse.over_limit = %q / %q", title, body)
	}
}

// A sentence with a hole in it never leaves the box. The catalogue promised
// the app "{kwh}" would be a number; an event that cannot supply one is an
// error, not a push.
func TestRenderPushRefusesAnUnfilledPlaceholder(t *testing.T) {
	if _, _, err := RenderPush(PushChargingSessionComplete, nil); err == nil {
		t.Fatal("rendered a sentence with {kwh} unfilled")
	}
	if _, _, err := RenderPush(PushDriverOffline, nil); err == nil {
		t.Fatal("rendered a sentence with {name} unfilled")
	}
	if _, _, err := RenderPush("charging.someday", nil); err == nil {
		t.Fatal("rendered a kind the catalogue does not carry")
	}
}

// The deadman payload must need no arguments: it is rendered once, encrypted,
// and handed to the relay long before the outage it describes. A placeholder
// in it could never be filled with a fact from the future.
func TestBoxUnreachableNeedsNoArguments(t *testing.T) {
	if _, _, err := RenderPush(PushBoxUnreachable, nil); err != nil {
		t.Fatalf("box.unreachable must render with no args: %v", err)
	}
}
