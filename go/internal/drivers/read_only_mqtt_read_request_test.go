package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Victron's Venus OS publishes its values only while a client sends a read
// request to R/<portal ID>/keepalive. A signed read-only driver that may
// subscribe may send the read requests its signed policy declares, and nothing
// else. It needs no write permission, so a Core that predates the field runs
// the same policy and refuses the request.

func readRequestPolicy() *RuntimePolicy {
	return &RuntimePolicy{
		PackageID: "com.sourceful.driver.victron_mqtt", Version: "1.2.0",
		ArtifactSHA256: strings.Repeat("c", 64), RuntimeABI: "gopher-lua-source-v1",
		HostAPIProfile: "sourceful.host/ftw-core/v1", ReadOnly: true,
		Permissions:           map[string]bool{"mqtt.subscribe": true},
		MQTTReadRequestTopics: []string{"R/+/keepalive"},
	}
}

func TestReadOnlyPolicyValidatesMQTTReadRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*RuntimePolicy)
		ok     bool
	}{
		{"keepalive", func(*RuntimePolicy) {}, true},
		{"several_topics", func(p *RuntimePolicy) {
			p.MQTTReadRequestTopics = []string{"R/+/keepalive", "R/+/system/0/Serial"}
		}, true},
		{"literal_topic", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R/c0619ab1234/keepalive"} }, true},
		// Declared topics without mqtt.subscribe open nothing.
		{"topics_without_subscribe", func(p *RuntimePolicy) { delete(p.Permissions, "mqtt.subscribe") }, true},
		// A read request needs no write permission, and mqtt.publish stays a
		// write: refused with or without declared topics.
		{"publish_with_topics", func(p *RuntimePolicy) { p.Permissions["mqtt.publish"] = true }, false},
		{"publish_without_topics", func(p *RuntimePolicy) {
			p.Permissions["mqtt.publish"] = true
			p.MQTTReadRequestTopics = nil
		}, false},
		{"multi_level_wildcard", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R/#"} }, false},
		{"bare_hash", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"#"} }, false},
		{"hash_inside_segment", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R/+/keep#alive"} }, false},
		{"partial_plus", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R/+x/keepalive"} }, false},
		{"wildcard_first_segment", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"+/+/keepalive"} }, false},
		{"empty_segment", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R//keepalive"} }, false},
		{"trailing_slash", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R/+/"} }, false},
		{"leading_slash", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"/R/keepalive"} }, false},
		{"broker_system_topic", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"$SYS/+/keepalive"} }, false},
		// The first level must be R, Venus's read-request namespace. A literal
		// first level is not enough: W/ is Venus's write namespace.
		{"venus_write", func(p *RuntimePolicy) {
			p.MQTTReadRequestTopics = []string{"W/+/settings/0/Settings/CGwacs/AcPowerSetPoint"}
		}, false},
		{"write_namespace", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"W/+/x"} }, false},
		{"other_namespace", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"X/+/keepalive"} }, false},
		{"lowercase_namespace", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"r/+/keepalive"} }, false},
		{"namespace_only", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R"} }, false},
		{"single_level", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"keepalive"} }, false},
		{"space", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R/+/keep alive"} }, false},
		{"control_character", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R/\x00/keepalive"} }, false},
		{"empty_topic", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R/+/keepalive", ""} }, false},
		{"too_long", func(p *RuntimePolicy) { p.MQTTReadRequestTopics = []string{"R/+/" + strings.Repeat("a", 253)} }, false},
		// An invalid pattern is refused even where it could not be used, so a
		// malformed signed entry fails at start instead of lying dormant.
		{"invalid_topic_without_subscribe", func(p *RuntimePolicy) {
			delete(p.Permissions, "mqtt.subscribe")
			p.MQTTReadRequestTopics = []string{"R/#"}
		}, false},
		{"not_read_only", func(p *RuntimePolicy) { p.ReadOnly = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := readRequestPolicy()
			tc.change(policy)
			err := policy.validate()
			if tc.ok && err != nil {
				t.Fatalf("validate refused %+v: %v", policy, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validate accepted %+v", policy)
			}
		})
	}
	// The limit is exactly 256 bytes, the same as the published topic's.
	policy := readRequestPolicy()
	policy.MQTTReadRequestTopics = []string{"R/+/" + strings.Repeat("a", 252)}
	if err := policy.validate(); err != nil {
		t.Fatalf("a 256-byte pattern was refused: %v", err)
	}
}

func TestMQTTReadRequestMatchesSegmentBySegment(t *testing.T) {
	for _, tc := range []struct {
		topic string
		ok    bool
	}{
		{"R/abc123/keepalive", true},
		{"R/c0619ab1234/keepalive", true},
		{"W/abc123/keepalive", false},
		{"R/abc123/keepalive/x", false},
		{"R/abc123", false},
		{"R/abc123/keepalivex", false},
		{"r/abc123/keepalive", false},
		{"R//keepalive", false},
		{"R/+/keepalive", false},
		{"R/#/keepalive", false},
		{"R/ab#c/keepalive", false},
		{"R/a b/keepalive", false},
		{"R/abc123/keepalive\x00", false},
		{"/R/abc123/keepalive", false},
		{"R/" + strings.Repeat("a", 244) + "/keepalive", true},  // 256 bytes
		{"R/" + strings.Repeat("a", 245) + "/keepalive", false}, // 257 bytes
		{"", false},
	} {
		if got := matchMQTTReadRequest("R/+/keepalive", tc.topic); got != tc.ok {
			t.Errorf("match(R/+/keepalive, %q) = %v, want %v", tc.topic, got, tc.ok)
		}
	}
	if !matchMQTTReadRequest("R/c0619ab1234/keepalive", "R/c0619ab1234/keepalive") ||
		matchMQTTReadRequest("R/c0619ab1234/keepalive", "R/other/keepalive") {
		t.Error("a literal pattern must match only itself")
	}
	// A pattern that validate would refuse matches nothing, so a policy that
	// skipped validation still cannot open a wider path.
	for _, pattern := range []string{"#", "R/#", "+/+/keepalive", "+/abc123/keepalive", "W/+/keepalive", "R/+x/keepalive", ""} {
		if matchMQTTReadRequest(pattern, "R/abc123/keepalive") {
			t.Errorf("invalid pattern %q matched", pattern)
		}
	}
}

type readRequestClock struct{ now time.Time }

func (c *readRequestClock) Now() time.Time { return c.now }

// loadPublishingDriver runs a driver whose init sends each publish in order
// and keeps what host.mqtt_publish returned, "sent" when it returned nothing.
func loadPublishingDriver(t *testing.T, env *HostEnv, policy *RuntimePolicy, publishes ...string) *LuaDriver {
	t.Helper()
	source := `results = {}
function send(topic, payload)
    results[#results + 1] = host.mqtt_publish(topic, payload) or "sent"
end
function driver_init(config)
` + strings.Join(publishes, "\n") + `
end
function driver_poll()
    send("R/abc123/keepalive", "")
    return 1000
end
`
	path := filepath.Join(t.TempDir(), "publisher.lua")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	driver, err := NewLuaDriverWithPolicy(path, env, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(driver.Cleanup)
	if err := driver.Init(context.Background(), map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	return driver
}

func publishResults(driver *LuaDriver) []string {
	results, ok := driver.L.GetGlobal("results").(*lua.LTable)
	if !ok {
		return nil
	}
	var out []string
	for i := 1; i <= results.Len(); i++ {
		out = append(out, results.RawGetInt(i).String())
	}
	return out
}

func TestManagedReadOnlyDriverSendsOnlyDeclaredReadRequests(t *testing.T) {
	for _, tc := range []struct {
		name    string
		send    string
		policy  func(*RuntimePolicy)
		env     func(*HostEnv)
		sent    bool
		refusal string
	}{
		{name: "keepalive_suppress_republish", send: `send("R/abc123/keepalive", '{"keepalive-options":["suppress-republish"]}')`, sent: true},
		{name: "keepalive_full_republish", send: `send("R/abc123/keepalive", "")`, sent: true},
		{name: "payload_at_limit", send: `send("R/abc123/keepalive", string.rep("x", 1024))`, sent: true},
		{name: "write_request", send: `send("W/abc123/settings/0/Settings/CGwacs/AcPowerSetPoint", '{"value":-5000}')`, refusal: "read-only driver cannot write"},
		{name: "another_read_topic", send: `send("R/abc123/system/0/Serial", "")`, refusal: "read-only driver cannot write"},
		{name: "deeper_topic", send: `send("R/abc123/keepalive/x", "")`, refusal: "read-only driver cannot write"},
		{name: "multi_level_wildcard", send: `send("R/#", "")`, refusal: "read-only driver cannot write"},
		{name: "wildcard_in_segment", send: `send("R/#/keepalive", "")`, refusal: "read-only driver cannot write"},
		{name: "plus_in_topic", send: `send("R/+/keepalive", "")`, refusal: "read-only driver cannot write"},
		{name: "empty_portal_id", send: `send("R//keepalive", "")`, refusal: "read-only driver cannot write"},
		{name: "oversized_payload", send: `send("R/abc123/keepalive", string.rep("x", 1025))`, refusal: "1024 bytes"},
		{name: "no_subscribe_permission", send: `send("R/abc123/keepalive", "")`,
			policy: func(p *RuntimePolicy) { delete(p.Permissions, "mqtt.subscribe") }, refusal: "read-only driver cannot write"},
		{name: "no_declared_topics", send: `send("R/abc123/keepalive", "")`,
			policy: func(p *RuntimePolicy) { p.MQTTReadRequestTopics = nil }, refusal: "read-only driver cannot write"},
		// A read request still meets the gates every write meets: setup
		// measurement mode holds it, and a fingerprint probe never sends one.
		{name: "setup_measurement_mode", send: `send("R/abc123/keepalive", "")`,
			env: func(env *HostEnv) { env.ControlBlocked = func() bool { return true } }, refusal: "setup measurement mode"},
		{name: "fingerprint_probe", send: `send("R/abc123/keepalive", "")`,
			env: func(env *HostEnv) { env.ProbeReadOnly = true }, refusal: "fingerprint probe"},
		{name: "setup_measurement_mode_write", send: `send("W/abc123/settings/0/Settings/CGwacs/AcPowerSetPoint", "{}")`,
			env: func(env *HostEnv) { env.ControlBlocked = func() bool { return true } }, refusal: "setup measurement mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mqtt := &fakeMQTT{}
			env := NewHostEnv("victron_mqtt", telemetry.NewStore()).WithMQTT(mqtt)
			if tc.env != nil {
				tc.env(env)
			}
			policy := readRequestPolicy()
			if tc.policy != nil {
				tc.policy(policy)
			}
			driver := loadPublishingDriver(t, env, policy, tc.send)
			got := publishResults(driver)
			if len(got) != 1 {
				t.Fatalf("results = %q", got)
			}
			published := mqtt.Published()
			if tc.sent {
				if got[0] != "sent" || len(published) != 1 {
					t.Fatalf("read request refused: %q, published %d", got[0], len(published))
				}
				return
			}
			if len(published) != 0 {
				t.Fatalf("refused publish reached the broker: %+v", published)
			}
			if !strings.Contains(got[0], tc.refusal) {
				t.Fatalf("refusal = %q, want it to mention %q", got[0], tc.refusal)
			}
		})
	}
}

// A declared topic must not become a write channel by volume: six read
// requests a minute, counted per driver over the last 60 s.
func TestManagedReadRequestsAreRateLimited(t *testing.T) {
	mqtt := &fakeMQTT{}
	clock := &readRequestClock{now: time.Unix(1_800_000_000, 0)}
	env := NewHostEnv("victron_mqtt", telemetry.NewStore()).WithMQTT(mqtt)
	env.readRequests.now = clock.Now
	sends := make([]string, 7)
	for i := range sends {
		sends[i] = `send("R/abc123/keepalive", "")`
	}
	driver := loadPublishingDriver(t, env, readRequestPolicy(), sends...)
	got := publishResults(driver)
	for i := 0; i < 6; i++ {
		if got[i] != "sent" {
			t.Fatalf("read request %d refused within the limit: %q", i+1, got[i])
		}
	}
	if !strings.Contains(got[6], "6 per minute") {
		t.Fatalf("seventh read request in a minute = %q, want the rate refusal", got[6])
	}
	if n := len(mqtt.Published()); n != 6 {
		t.Fatalf("broker received %d read requests, want 6", n)
	}

	// Still inside the window: refused, and a refusal does not count.
	clock.now = clock.now.Add(59 * time.Second)
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := publishResults(driver); !strings.Contains(got[7], "6 per minute") {
		t.Fatalf("read request 59 s later = %q, want refused", got[7])
	}
	// Once the oldest leaves the window, one more may go.
	clock.now = clock.now.Add(1 * time.Second)
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := publishResults(driver); got[8] != "sent" {
		t.Fatalf("read request after the window = %q, want sent", got[8])
	}
	// Victron's own cadence, one every 30 s, never meets the limit.
	for i := 0; i < 20; i++ {
		clock.now = clock.now.Add(30 * time.Second)
		if _, err := driver.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		results := publishResults(driver)
		if last := results[len(results)-1]; last != "sent" {
			t.Fatalf("keep-alive every 30 s refused at %d: %q", i, last)
		}
	}
}

// Bundled, local and control-capable signed drivers run without a policy and
// publish as before: any topic, any size, no rate limit. Setup measurement
// mode still blocks them.
func TestDriversWithoutPolicyPublishAsBefore(t *testing.T) {
	mqtt := &fakeMQTT{}
	env := NewHostEnv("bundled", telemetry.NewStore()).WithMQTT(mqtt)
	sends := []string{
		`send("W/abc123/settings/0/Settings/CGwacs/AcPowerSetPoint", '{"value":-5000}')`,
		`send("R/abc123/keepalive", string.rep("x", 2048))`,
	}
	for i := 0; i < 10; i++ {
		sends = append(sends, `send("R/abc123/keepalive", "")`)
	}
	driver := loadPublishingDriver(t, env, nil, sends...)
	for i, got := range publishResults(driver) {
		if got != "sent" {
			t.Fatalf("publish %d without a policy refused: %q", i+1, got)
		}
	}
	if n := len(mqtt.Published()); n != len(sends) {
		t.Fatalf("broker received %d publishes, want %d", n, len(sends))
	}

	blocked := &fakeMQTT{}
	env = NewHostEnv("bundled", telemetry.NewStore()).WithMQTT(blocked)
	env.ControlBlocked = func() bool { return true }
	driver = loadPublishingDriver(t, env, nil, `send("R/abc123/keepalive", "")`)
	if got := publishResults(driver); !strings.Contains(got[0], "setup measurement mode") || len(blocked.Published()) != 0 {
		t.Fatalf("setup measurement mode no longer blocks a driver without a policy: %q", got)
	}
}
