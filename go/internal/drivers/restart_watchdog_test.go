package drivers

import (
	"context"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const restartWatchSessionDriver = `
function driver_init(config)
  host.set_poll_interval(60000)
  if config ~= nil and config.fail == "yes" then
    error("start failed")
  end
  if config ~= nil and config.emit == "yes" then
    host.emit_metric("alive", 1)
  end
end
function driver_poll() return 60000 end
function driver_default_mode()
  host.emit_metric("default_mode", 1)
end
`

const restartWatchCloudDriver = `
DRIVER = {
  id = "cloud_login",
  protocols = { "http" },
  http_hosts = { "api.example.com" },
  auth_post_path = "/oauth/token",
}
function driver_init(config)
  host.set_poll_interval(60000)
  if config ~= nil and config.fail == "yes" then
    error("token rejected")
  end
end
function driver_poll() return 60000 end
`

const restartWatchPushDriver = `
function driver_init(config)
  host.set_poll_interval(20)
end
function driver_poll() return 20 end
`

func TestRestartBackoffSteps(t *testing.T) {
	want := []time.Duration{
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		5 * time.Minute,
		5 * time.Minute,
	}
	for step, delay := range want {
		if got := restartDelay(step); got != delay {
			t.Fatalf("step %d = %s, want %s", step, got, delay)
		}
	}
}

func TestCloudAndPushClassification(t *testing.T) {
	if !cloudOrOAuth(config.Driver{CredentialOwner: "owner"}, CatalogEntry{}) {
		t.Fatal("credential owner should be treated as OAuth")
	}
	if !cloudOrOAuth(config.Driver{}, CatalogEntry{AuthPostPath: "/oauth/token"}) {
		t.Fatal("auth_post_path should be treated as OAuth")
	}
	if !cloudOrOAuth(config.Driver{}, CatalogEntry{Protocols: []string{"http"}, HTTPHosts: []string{"api.easee.com"}}) {
		t.Fatal("vendor HTTP host should be treated as cloud")
	}
	if cloudOrOAuth(config.Driver{}, CatalogEntry{Protocols: []string{"http"}}) {
		t.Fatal("HTTP without a vendor host is a local device")
	}
	if cloudOrOAuth(config.Driver{}, CatalogEntry{Protocols: []string{"modbus", "http"}, HTTPHosts: []string{"api.example.com"}}) {
		t.Fatal("a modbus driver is not cloud-only")
	}
	mqtt := config.Driver{Capabilities: config.Capabilities{MQTT: &config.MQTTConfig{Host: "broker"}}}
	if !pushTransport(mqtt, CatalogEntry{}) {
		t.Fatal("MQTT capability should be a push transport")
	}
	if !pushTransport(config.Driver{}, CatalogEntry{Protocols: []string{"tcp"}}) {
		t.Fatal("TCP protocol should be a push transport")
	}
	if !pushTransport(config.Driver{}, CatalogEntry{Protocols: []string{"websocket"}}) {
		t.Fatal("websocket protocol should be a push transport")
	}
	if pushTransport(config.Driver{}, CatalogEntry{Protocols: []string{"modbus"}}) {
		t.Fatal("modbus is not a push transport")
	}
}

func TestRestartWatchdogRestartsOfflineDriverThenRecovers(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	path := writeTestDriver(t, restartWatchSessionDriver)
	cfg := config.Driver{Name: "battery", Lua: path, Config: map[string]any{"emit": "yes"}}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	markDriverDown(tel, "battery", now, 2*time.Minute)

	got := r.MaintainDrivers(context.Background(), now, []config.Driver{cfg}, time.Minute)
	if len(got) != 1 || got[0].Reason != "offline" || got[0].Err != nil {
		t.Fatalf("offline restart = %+v", got)
	}
	if _, _, ok := tel.LatestMetric("battery", "default_mode"); !ok {
		t.Fatal("restart did not run driver_default_mode")
	}
	health := tel.DriverHealth("battery")
	if health == nil || health.Status == telemetry.StatusOffline || health.LastSuccess == nil {
		t.Fatalf("restart did not come back with fresh telemetry: %+v", health)
	}
	if again := r.MaintainDrivers(context.Background(), now, []config.Driver{cfg}, time.Minute); len(again) != 0 {
		t.Fatalf("recovered driver restarted again: %+v", again)
	}

	// Backoff was reset by the recovery, so a new outage is eligible at 30s
	// of staleness rather than the next grown delay.
	markDriverStale(tel, "battery", now, 2*time.Minute)
	got = r.MaintainDrivers(context.Background(), now, []config.Driver{cfg}, time.Minute)
	if len(got) != 1 || got[0].Reason != "stale" || got[0].Err != nil {
		t.Fatalf("stale restart after recovery = %+v", got)
	}
	status, ok := r.RestartStatus("battery")
	if !ok || status.Restarts != 2 {
		t.Fatalf("status = %+v ok=%v, want 2 restarts", status, ok)
	}
}

func TestRestartWatchdogBackoffGrowsAndResets(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	path := writeTestDriver(t, restartWatchSessionDriver)
	cfg := config.Driver{Name: "battery", Lua: path, Config: map[string]any{"fail": "yes"}}
	base := time.Now()
	// Waits before each new attempt: 30s, 1m, 2m, 5m, then 5m again.
	steps := []struct {
		at   time.Duration
		want uint64
	}{
		{0, 0},
		{29 * time.Second, 0},
		{30 * time.Second, 1},
		{89 * time.Second, 1},
		{90 * time.Second, 2},
		{210*time.Second - time.Second, 2},
		{210 * time.Second, 3},
		{510*time.Second - time.Second, 3},
		{510 * time.Second, 4},
		{810*time.Second - time.Second, 4},
		{810 * time.Second, 5},
	}
	for _, step := range steps {
		r.MaintainDrivers(context.Background(), base.Add(step.at), []config.Driver{cfg}, time.Minute)
		if got := restartCount(r, "battery"); got != step.want {
			t.Fatalf("at %s restarts = %d, want %d", step.at, got, step.want)
		}
		if step.want > 0 && len(r.Names()) != 0 {
			t.Fatalf("at %s driver is running after a failed start", step.at)
		}
	}

	successAt := base.Add(810*time.Second + 5*time.Minute)
	cfg.Config = map[string]any{"emit": "yes"}
	if got := r.MaintainDrivers(context.Background(), successAt, []config.Driver{cfg}, time.Minute); len(got) != 1 || got[0].Err != nil {
		t.Fatalf("successful retry = %+v", got)
	}
	markDriverRecovered(tel, "battery", successAt)
	if got := r.MaintainDrivers(context.Background(), successAt, []config.Driver{cfg}, time.Minute); len(got) != 0 {
		t.Fatalf("recovered driver restarted: %+v", got)
	}

	r.Remove("battery")
	cfg.Config = map[string]any{"fail": "yes"}
	r.MaintainDrivers(context.Background(), successAt, []config.Driver{cfg}, time.Minute)
	if got := restartCount(r, "battery"); got != 6 {
		t.Fatalf("observation after recovery changed restarts to %d", got)
	}
	r.MaintainDrivers(context.Background(), successAt.Add(29*time.Second), []config.Driver{cfg}, time.Minute)
	if got := restartCount(r, "battery"); got != 6 {
		t.Fatalf("backoff reset waited %s, restarts = %d", 29*time.Second, got)
	}
	r.MaintainDrivers(context.Background(), successAt.Add(30*time.Second), []config.Driver{cfg}, time.Minute)
	if got := restartCount(r, "battery"); got != 7 {
		t.Fatalf("after recovery the next retry waited past 30s, restarts = %d", got)
	}
}

func TestRestartWatchdogRetriesStartFailure(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	path := writeTestDriver(t, restartWatchSessionDriver)
	cfg := config.Driver{Name: "battery", Lua: path, Config: map[string]any{"fail": "yes"}}
	now := time.Now()
	if got := r.MaintainDrivers(context.Background(), now, []config.Driver{cfg}, time.Minute); len(got) != 0 {
		t.Fatalf("first look retried immediately: %+v", got)
	}
	status, ok := r.RestartStatus("battery")
	if !ok || !status.NextAttempt.Equal(now.Add(30*time.Second)) || status.Restarts != 0 {
		t.Fatalf("pending retry = %+v ok=%v", status, ok)
	}
	got := r.MaintainDrivers(context.Background(), now.Add(30*time.Second), []config.Driver{cfg}, time.Minute)
	if len(got) != 1 || got[0].Reason != "start_failed" || got[0].Err == nil {
		t.Fatalf("start retry = %+v", got)
	}
	if len(r.Names()) != 0 {
		t.Fatalf("failed start left a running driver: %v", r.Names())
	}
	status, ok = r.RestartStatus("battery")
	if !ok || status.Restarts != 1 || status.LastReason != "start_failed" || status.LastError == "" {
		t.Fatalf("status after failed start = %+v ok=%v", status, ok)
	}
}

func TestRestartWatchdogLeavesDisabledAndObserveOnly(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	path := writeTestDriver(t, restartWatchSessionDriver)
	now := time.Now()
	disabled := config.Driver{Name: "battery", Lua: path, Disabled: true, Config: map[string]any{"emit": "yes"}}
	if got := r.MaintainDrivers(context.Background(), now.Add(time.Hour), []config.Driver{disabled}, time.Minute); len(got) != 0 || len(r.Names()) != 0 {
		t.Fatalf("disabled driver was started: events=%+v names=%v", got, r.Names())
	}

	enabled := disabled
	enabled.Disabled = false
	if err := r.Add(context.Background(), enabled); err != nil {
		t.Fatal(err)
	}
	markDriverDown(tel, "battery", now, 10*time.Minute)
	if got := r.MaintainDrivers(context.Background(), now, []config.Driver{disabled}, time.Minute); len(got) != 0 {
		t.Fatalf("disabled running driver was restarted: %+v", got)
	}
	if len(r.Names()) != 1 {
		t.Fatalf("disabled entry stopped the driver: %v", r.Names())
	}

	r.Remove("battery")
	observe := config.Driver{Name: "battery", Lua: path, ObserveOnly: true, Config: map[string]any{"emit": "yes"}}
	if err := r.Add(context.Background(), observe); err != nil {
		t.Fatal(err)
	}
	markDriverDown(tel, "battery", now, 10*time.Minute)
	if got := r.MaintainDrivers(context.Background(), now, []config.Driver{observe}, time.Minute); len(got) != 0 {
		t.Fatalf("observe-only driver was restarted: %+v", got)
	}
	if _, ok := r.RestartStatus("battery"); ok {
		t.Fatal("observe-only driver has restart state")
	}
}

func TestRestartWatchdogDoesNotReloginCloudDriver(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	path := writeTestDriver(t, restartWatchCloudDriver)
	cfg := config.Driver{Name: "myuplink", Lua: path}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	markDriverDown(tel, "myuplink", now, 30*time.Minute)
	if got := r.MaintainDrivers(context.Background(), now.Add(time.Hour), []config.Driver{cfg}, time.Minute); len(got) != 0 {
		t.Fatalf("stale cloud driver was restarted: %+v", got)
	}
	if restartCount(r, "myuplink") != 0 || len(r.Names()) != 1 {
		t.Fatalf("cloud driver restarts=%d names=%v", restartCount(r, "myuplink"), r.Names())
	}

	r.Remove("myuplink")
	cfg.Config = map[string]any{"fail": "yes"}
	base := time.Now()
	r.MaintainDrivers(context.Background(), base, []config.Driver{cfg}, time.Minute)
	got := r.MaintainDrivers(context.Background(), base.Add(30*time.Second), []config.Driver{cfg}, time.Minute)
	if len(got) != 1 || got[0].Reason != "start_failed" || got[0].Err == nil {
		t.Fatalf("cloud start retry = %+v", got)
	}
}

func TestRestartWatchdogLeavesMQTTWaitingForData(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	path := writeTestDriver(t, restartWatchPushDriver)
	cfg := config.Driver{
		Name: "ferroamp",
		Lua:  path,
		Capabilities: config.Capabilities{
			MQTT: &config.MQTTConfig{Host: "127.0.0.1", Port: 1883},
		},
	}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		health := tel.DriverHealth("ferroamp")
		if health != nil && health.TickCount > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mqtt driver never polled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	now := time.Now()
	health := tel.DriverHealthMut("ferroamp")
	health.StartedAt = now.Add(-10 * time.Minute)
	health.LastSuccess = nil
	health.ConsecutiveErrors = 0
	health.LastError = ""
	health.SetOffline()
	if got := r.MaintainDrivers(context.Background(), now, []config.Driver{cfg}, time.Minute); len(got) != 0 {
		t.Fatalf("mqtt driver waiting for data was restarted: %+v", got)
	}

	past := now.Add(-10 * time.Minute)
	health = tel.DriverHealthMut("ferroamp")
	health.StartedAt = past
	health.LastSuccess = &past
	health.SetOffline()
	got := r.MaintainDrivers(context.Background(), now, []config.Driver{cfg}, time.Minute)
	if len(got) != 1 || got[0].Err != nil {
		t.Fatalf("mqtt driver that had data and went stale was not restarted: %+v", got)
	}
}

func TestRestartWatchdogStopsWhenContextCancelled(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	path := writeTestDriver(t, restartWatchSessionDriver)
	cfg := config.Driver{Name: "battery", Lua: path, Config: map[string]any{"emit": "yes"}}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	markDriverDown(tel, "battery", now, 10*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := r.MaintainDrivers(ctx, now, []config.Driver{cfg}, time.Minute); len(got) != 0 {
		t.Fatalf("cancelled watchdog restarted: %+v", got)
	}
	if restartCount(r, "battery") != 0 || len(r.Names()) != 1 {
		t.Fatalf("cancelled watchdog changed the driver: restarts=%d names=%v", restartCount(r, "battery"), r.Names())
	}
}

func TestRestartWatchdogWaitsForLifecycleLock(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	path := writeTestDriver(t, restartWatchSessionDriver)
	cfg := config.Driver{Name: "battery", Lua: path, Config: map[string]any{"emit": "yes"}}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	markDriverDown(tel, "battery", now, 10*time.Minute)
	release := r.acquireLifecycle("battery")
	released := false
	t.Cleanup(func() {
		if !released {
			release()
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.MaintainDrivers(context.Background(), now, []config.Driver{cfg}, time.Minute)
	}()
	select {
	case <-done:
		t.Fatal("watchdog restarted while the driver lifecycle lock was held")
	case <-time.After(200 * time.Millisecond):
	}
	if restartCount(r, "battery") != 0 {
		t.Fatal("restart counted while lifecycle lock was held")
	}
	release()
	released = true
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not restart after the lifecycle lock was released")
	}
	if restartCount(r, "battery") != 1 {
		t.Fatalf("restarts = %d, want 1", restartCount(r, "battery"))
	}
}

func markDriverDown(tel *telemetry.Store, name string, now time.Time, age time.Duration) {
	health := tel.DriverHealthMut(name)
	past := now.Add(-age)
	health.StartedAt = past
	health.LastSuccess = &past
	health.SetOffline()
	health.ConsecutiveErrors = 0
	health.LastError = ""
}

func markDriverStale(tel *telemetry.Store, name string, now time.Time, age time.Duration) {
	health := tel.DriverHealthMut(name)
	past := now.Add(-age)
	health.StartedAt = past
	health.LastSuccess = &past
	health.Status = telemetry.StatusOk
	health.ConsecutiveErrors = 0
	health.LastError = ""
}

func markDriverRecovered(tel *telemetry.Store, name string, at time.Time) {
	health := tel.DriverHealthMut(name)
	health.StartedAt = at
	success := at
	health.LastSuccess = &success
	health.Status = telemetry.StatusOk
	health.ConsecutiveErrors = 0
	health.LastError = ""
}

func restartCount(r *Registry, name string) uint64 {
	status, ok := r.RestartStatus(name)
	if !ok {
		return 0
	}
	return status.Restarts
}
