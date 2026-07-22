package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	controlPlaneHelperUpdater  = "updater"
	controlPlaneHelperNormal   = "normal"
	controlPlaneHelperRecovery = "recovery"
)

type controlPlaneRelease struct {
	Target        string
	Revision      string
	CoreDigest    string
	UpdaterDigest string
}

func (r controlPlaneRelease) validate() error {
	if !isImmutableImageTag(r.Target) {
		return errors.New("target is not an immutable release tag")
	}
	if !validHexValue(r.Revision, 40, "") {
		return errors.New("source revision must contain 40 lowercase hex characters")
	}
	if !validHexValue(r.CoreDigest, 64, "sha256:") || !validHexValue(r.UpdaterDigest, 64, "sha256:") {
		return errors.New("Core and updater digests must be sha256 values")
	}
	return nil
}

func validHexValue(value string, digits int, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+digits {
		return false
	}
	for _, r := range value[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func (r controlPlaneRelease) coreRef() string {
	return canonicalMainImage + "@" + r.CoreDigest
}

func (r controlPlaneRelease) updaterRef() string {
	return canonicalUpdaterImage + "@" + r.UpdaterDigest
}

func controlPlaneEnv(target string) []string {
	return []string{"FTW_IMAGE_TAG=" + target, "FTW_UPDATER_IMAGE_TAG=" + target}
}

func (s *server) startControlPlaneHeartbeat(target, transactionID, helperKind string, states ...string) func() {
	allowed := make(map[string]bool, len(states))
	for _, state := range states {
		allowed[state] = true
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.refreshControlPlaneHeartbeat(target, transactionID, helperKind, allowed)
			case <-stop:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

func (s *server) refreshControlPlaneHeartbeat(target, transactionID, helperKind string, allowed map[string]bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	st := s.readStateLocked()
	if s.heartbeatAfterRead != nil {
		s.heartbeatAfterRead()
	}
	if st.Action == "update" && st.Component == "core" && st.Target == target &&
		st.TransactionID == transactionID && st.HelperKind == helperKind && allowed[st.State] {
		st.UpdatedAt = s.nowTime()
		s.writeStateLocked(st)
	}
}

// runControlPlaneUpdate captures both rollback images and pulls the matched
// pair. A detached helper then owns replacement and rollback while the updater
// service replaces itself.
func (s *server) runControlPlaneUpdate(release controlPlaneRelease, startedAt time.Time) {
	target := release.Target
	transactionID := newControlPlaneTransactionID(startedAt)
	stopHeartbeat := s.startControlPlaneHeartbeat(target, transactionID, controlPlaneHelperUpdater, "pulling")
	defer stopHeartbeat()
	base := State{
		State: "pulling", Action: "update", Component: "core", Target: target,
		StartedAt: startedAt, UpdatedAt: s.nowTime(), ReleaseRevision: release.Revision,
		CoreDigest: release.CoreDigest, UpdaterDigest: release.UpdaterDigest, TransactionID: transactionID,
		HelperKind: controlPlaneHelperUpdater,
	}
	s.writeState(base)
	if err := release.validate(); err != nil {
		s.failControlPlanePreflight(base, err)
		return
	}

	cleanup, err := s.prepareControlPlaneDigestPins(release)
	if err != nil {
		s.failControlPlanePreflight(base, err)
		return
	}
	cleanupActive := true
	defer func() {
		if cleanupActive {
			cleanup()
		}
	}()
	if err := s.validateControlPlaneDigestPins(release); err != nil {
		s.failControlPlanePreflight(base, err)
		return
	}
	specs, err := s.controlPlaneSpecs()
	if err != nil {
		base.State, base.Message, base.UpdatedAt = "failed", err.Error(), s.nowTime()
		s.writeState(base)
		return
	}

	previous := make(map[string]string, len(specs))
	for _, spec := range specs {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		imageID, captureErr := s.imageID(ctx, spec.service)
		cancel()
		if captureErr != nil || imageID == "" {
			if captureErr == nil {
				captureErr = errors.New("empty image ID")
			}
			base.State = "failed"
			base.Message = "cannot capture " + spec.name + " image for rollback: " + captureErr.Error()
			base.UpdatedAt = s.nowTime()
			s.writeState(base)
			return
		}
		previous[spec.name] = imageID
	}
	base.PreviousImages = previous
	base.PreviousImageID = previous["core"]
	s.writeState(base)

	if !s.skipPull {
		pullArgs := s.composeArgs("pull", s.mainServiceName, updaterServiceName)
		var pullErr error
		for attempt := 1; ; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
			pullErr = s.runner(ctx, controlPlaneEnv(target), pullArgs...)
			cancel()
			if pullErr == nil {
				break
			}
			if s.maxPullAttempts > 0 && attempt >= s.maxPullAttempts {
				break
			}
			time.Sleep(s.pullRetryDelay)
		}
		if pullErr != nil {
			base.State, base.Message, base.UpdatedAt = "failed", "digest-pinned pair pull failed: "+pullErr.Error(), s.nowTime()
			s.writeState(base)
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	updaterContainer, err := s.containerID(ctx, updaterServiceName)
	cancel()
	if err != nil {
		base.State, base.Message, base.UpdatedAt = "failed", "cannot locate updater container: "+err.Error(), s.nowTime()
		s.writeState(base)
		return
	}
	// The pull is complete. Drop this process's private override before the
	// detached helper creates its own digest-pinned override.
	cleanup()
	cleanupActive = false
	// The detached helper becomes the sole state writer at handoff. Wait for
	// this updater's heartbeat before publishing helper ownership.
	stopHeartbeat()
	base.State = "transacting"
	base.HelperKind = controlPlaneHelperNormal
	base.Message = "paired images pulled; detached helper owns the transaction"
	base.UpdatedAt = s.nowTime()
	s.writeState(base)
	if s.launchTransaction != nil {
		if err := s.launchTransaction(release, previous, startedAt, transactionID); err != nil {
			base.State, base.Message, base.UpdatedAt = "failed", "cannot start transaction helper: "+err.Error(), s.nowTime()
			s.writeState(base)
		}
		return
	}
	err = s.launchControlPlaneHelper(updaterContainer, previous["updater"], false, release, previous, startedAt, transactionID)
	if err != nil {
		base.State, base.Message, base.UpdatedAt = "failed", "cannot start transaction helper: "+err.Error(), s.nowTime()
		s.writeState(base)
	}
}

func (s *server) nowTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func newControlPlaneTransactionID(startedAt time.Time) string {
	return fmt.Sprintf("%016x", uint64(startedAt.UnixNano()))
}

func validControlPlaneTransactionID(transactionID string) bool {
	return validHexValue(transactionID, len(transactionID), "") && len(transactionID) >= 16 && len(transactionID) <= 64
}

func controlPlaneHelperName(recovery bool, transactionID string) string {
	prefix := "ftw-control-plane-"
	if recovery {
		prefix += "recovery-"
	}
	return prefix + transactionID
}

func removeControlPlaneTransactionHelper(transactionID string, recovery bool) error {
	if !validControlPlaneTransactionID(transactionID) {
		return errors.New("invalid paired transaction ID")
	}
	name := controlPlaneHelperName(recovery, transactionID)
	inspectCtx, cancelInspect := context.WithTimeout(context.Background(), 30*time.Second)
	_, err := dockerOutput(inspectCtx, "container", "inspect", "--format", "{{.Id}}", name)
	cancelInspect()
	if err != nil {
		if strings.Contains(err.Error(), "No such object") || strings.Contains(err.Error(), "No such container") {
			return nil
		}
		return fmt.Errorf("inspect transaction helper %s: %w", name, err)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStop()
	if err := dockerCompose(stopCtx, nil, "rm", "-f", name); err != nil {
		return fmt.Errorf("stop transaction helper %s: %w", name, err)
	}
	return nil
}

func (s *server) launchControlPlaneHelper(updaterContainer, helperImage string, recovery bool, release controlPlaneRelease, previous map[string]string, startedAt time.Time, transactionID string) error {
	if !validControlPlaneTransactionID(transactionID) {
		return errors.New("invalid paired transaction ID")
	}
	args := []string{
		"run", "--rm", "--detach", "--name", controlPlaneHelperName(recovery, transactionID),
		"--network", "none", "--volumes-from", updaterContainer,
	}
	for _, pair := range []string{
		"FTW_UPDATER_COMPOSE=" + s.composeFile,
		"FTW_UPDATER_MAIN_SERVICE=" + s.mainServiceName,
		"FTW_UPDATER_SOCKET=" + s.socketPath,
		"FTW_UPDATER_STATUS=" + s.statusPath,
		"COMPOSE_PROJECT_NAME=" + os.Getenv("COMPOSE_PROJECT_NAME"),
	} {
		args = append(args, "--env", pair)
	}
	args = append(args, helperImage)
	if recovery {
		args = append(args, "-control-plane-recovery")
	}
	args = append(args,
		"-control-plane-transaction", release.Target,
		"-previous-core-image", previous["core"],
		"-previous-updater-image", previous["updater"],
		"-transaction-started-at", startedAt.Format(time.RFC3339Nano),
		"-transaction-id", transactionID,
		"-release-revision", release.Revision,
		"-core-digest", release.CoreDigest,
		"-updater-digest", release.UpdaterDigest,
	)
	launchCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return s.runner(launchCtx, nil, args...)
}

func (s *server) monitorControlPlaneTransaction() {
	poll := s.recoveryPoll
	if poll <= 0 {
		poll = controlPlaneRecoveryPoll
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for range ticker.C {
		if s.recoverStaleControlPlaneTransaction() {
			return
		}
		if st := s.readState(); !controlPlaneRecoveryInFlight(st) {
			return
		}
	}
}

// recoverStaleControlPlaneTransaction claims a transaction only after its
// durable heartbeat lease expires. It reads the lease again just before the
// claim so a current heartbeat can stop recovery.
func (s *server) recoverStaleControlPlaneTransaction() bool {
	st := s.readState()
	if !controlPlaneRecoveryInFlight(st) {
		return true
	}
	ttl := s.transactionTTL
	if ttl <= 0 {
		ttl = controlPlaneStaleAfter
	}
	if st.UpdatedAt.IsZero() || s.nowTime().Sub(st.UpdatedAt) <= ttl {
		return false
	}
	if !s.runMu.TryLock() {
		return false
	}
	defer s.runMu.Unlock()

	st = s.readState()
	if !controlPlaneRecoveryInFlight(st) {
		return true
	}
	if st.UpdatedAt.IsZero() || s.nowTime().Sub(st.UpdatedAt) <= ttl {
		return false
	}
	if st.PreviousImages["core"] == "" || st.PreviousImages["updater"] == "" {
		st.State = "failed"
		st.Message = "stale control-plane transaction has no complete rollback image pair; manual recovery required"
		st.UpdatedAt = s.nowTime()
		s.writeState(st)
		return true
	}
	if !validControlPlaneTransactionID(st.TransactionID) {
		st.State = "failed"
		st.Message = "stale control-plane transaction has no valid transaction ID; manual recovery required"
		st.UpdatedAt = s.nowTime()
		s.writeState(st)
		return true
	}

	var recoveryHelper bool
	switch st.HelperKind {
	case controlPlaneHelperNormal:
		recoveryHelper = false
	case controlPlaneHelperRecovery:
		recoveryHelper = true
	default:
		st.State = "failed"
		st.Message = "stale control-plane transaction has no valid helper owner; manual recovery required"
		st.UpdatedAt = s.nowTime()
		s.writeState(st)
		return true
	}
	if s.stopTransactionHelper == nil {
		current := s.readState()
		if current.TransactionID != st.TransactionID || current.HelperKind != st.HelperKind || !controlPlaneRecoveryInFlight(current) {
			return true
		}
		current.State = "failed"
		current.Message = "detached helper heartbeat expired but helper fencing is unavailable; manual recovery required"
		current.UpdatedAt = s.nowTime()
		s.writeState(current)
		return true
	}
	stopErr := s.stopTransactionHelper(st.TransactionID, recoveryHelper)
	// The helper may have written a terminal result while Docker stopped it.
	// Keep that result. Otherwise create the recovery claim after the helper
	// can no longer overwrite it.
	current := s.readState()
	if current.TransactionID != st.TransactionID || current.HelperKind != st.HelperKind || !controlPlaneRecoveryInFlight(current) {
		return true
	}
	if stopErr != nil {
		current.State = "failed"
		current.Message = "detached helper heartbeat expired but it could not be stopped; manual recovery required: " + stopErr.Error()
		current.UpdatedAt = s.nowTime()
		s.writeState(current)
		return true
	}
	current.State = "restoring"
	current.HelperKind = controlPlaneHelperRecovery
	current.Message = "detached helper stopped; launching updater-first pair recovery"
	current.UpdatedAt = s.nowTime()
	s.writeState(current)
	var err error
	if s.launchRecovery != nil {
		err = s.launchRecovery(current)
	} else {
		err = s.launchControlPlaneRecoveryHelper(current)
	}
	if err != nil {
		current.State = "failed"
		current.Message = "detached helper heartbeat expired and recovery helper could not start; pair may be mixed: " + err.Error()
		current.UpdatedAt = s.nowTime()
		s.writeState(current)
	}
	return true
}

func (s *server) launchControlPlaneRecoveryHelper(st State) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	updaterContainer, err := s.containerID(ctx, updaterServiceName)
	cancel()
	if err != nil {
		return fmt.Errorf("locate updater for recovery: %w", err)
	}
	release := controlPlaneRelease{
		Target: st.Target, Revision: st.ReleaseRevision,
		CoreDigest: st.CoreDigest, UpdaterDigest: st.UpdaterDigest,
	}
	return s.launchControlPlaneHelper(updaterContainer, st.PreviousImages["updater"], true, release, st.PreviousImages, st.StartedAt, st.TransactionID)
}

func (s *server) runControlPlaneRecovery(st State) {
	st.HelperKind = controlPlaneHelperRecovery
	stopHeartbeat := s.startControlPlaneHeartbeat(st.Target, st.TransactionID, controlPlaneHelperRecovery, "restoring")
	defer stopHeartbeat()
	s.rollbackControlPlane(st, errors.New("detached transaction helper stopped heartbeating"), stopHeartbeat)
}

func controlPlaneRecoveryInFlight(st State) bool {
	return st.Action == "update" && st.Component == "core" &&
		(st.State == "transacting" || st.State == "restoring")
}

func (s *server) failControlPlanePreflight(base State, err error) {
	base.State = "failed"
	base.Message = "compose preflight failed: " + err.Error()
	base.UpdatedAt = s.nowTime()
	s.writeState(base)
}

func (s *server) runControlPlaneTransaction(release controlPlaneRelease, previous map[string]string, startedAt time.Time, transactionID string) {
	stopHeartbeat := s.startControlPlaneHeartbeat(release.Target, transactionID, controlPlaneHelperNormal, "transacting", "restoring")
	defer stopHeartbeat()
	base := State{
		State: "transacting", Action: "update", Component: "core", Target: release.Target,
		StartedAt: startedAt, UpdatedAt: s.nowTime(), PreviousImageID: previous["core"], PreviousImages: previous,
		ReleaseRevision: release.Revision, CoreDigest: release.CoreDigest, UpdaterDigest: release.UpdaterDigest,
		TransactionID: transactionID, HelperKind: controlPlaneHelperNormal,
	}
	cleanup := func() {}
	cleanupActive := false
	fail := func(err error) {
		if cleanupActive {
			cleanup()
			cleanupActive = false
		}
		s.rollbackControlPlane(base, err, stopHeartbeat)
	}
	if err := release.validate(); err != nil || previous["core"] == "" || previous["updater"] == "" || !validControlPlaneTransactionID(transactionID) {
		if err == nil {
			switch {
			case previous["core"] == "" || previous["updater"] == "":
				err = errors.New("previous Core and updater image IDs are required")
			default:
				err = errors.New("valid transaction ID is required")
			}
		}
		fail(fmt.Errorf("invalid paired transaction parameters: %w", err))
		return
	}
	var err error
	cleanup, err = s.prepareControlPlaneDigestPins(release)
	if err != nil {
		fail(fmt.Errorf("compose preflight: %w", err))
		return
	}
	cleanupActive = true
	defer func() {
		if cleanupActive {
			cleanup()
		}
	}()
	if err := s.validateControlPlaneDigestPins(release); err != nil {
		fail(fmt.Errorf("compose preflight: %w", err))
		return
	}
	env := controlPlaneEnv(release.Target)

	base.Message = "replacing updater with matching release"
	base.UpdatedAt = s.nowTime()
	s.writeState(base)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	err = s.runner(ctx, env, s.composeArgs("up", "-d", updaterServiceName)...)
	cancel()
	if err != nil {
		fail(fmt.Errorf("replace updater: %w", err))
		return
	}
	compatCtx, cancelCompat := context.WithTimeout(context.Background(), 3*time.Minute)
	if s.updaterReady != nil {
		err = s.updaterReady(compatCtx, release.Target)
	} else {
		err = s.waitForUpdaterRelease(compatCtx, release.Target)
	}
	cancelCompat()
	if err != nil {
		fail(err)
		return
	}
	if err := s.requireServiceImageRef(updaterServiceName, release.updaterRef()); err != nil {
		fail(err)
		return
	}

	base.Message = "matching updater ready; replacing Core"
	base.UpdatedAt = s.nowTime()
	s.writeState(base)
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Minute)
	err = s.runner(ctx, env, s.composeArgs("up", "-d", s.mainServiceName)...)
	cancel()
	if err != nil {
		fail(fmt.Errorf("replace Core: %w", err))
		return
	}
	if s.healthCheck != nil {
		healthCtx, cancelHealth := context.WithTimeout(context.Background(), componentHealthTimeout("core"))
		err = s.healthCheck(healthCtx, s.mainServiceName)
		cancelHealth()
		if err != nil {
			fail(fmt.Errorf("new Core readiness: %w", err))
			return
		}
	}
	if err := s.requireServiceImageRef(s.mainServiceName, release.coreRef()); err != nil {
		fail(err)
		return
	}
	if err := s.requireServiceImageRef(updaterServiceName, release.updaterRef()); err != nil {
		fail(err)
		return
	}
	base.State = "done"
	base.Message = "Core and updater committed as one digest-locked release pair"
	base.UpdatedAt = s.nowTime()
	stopHeartbeat()
	s.writeState(base)
}

func (s *server) waitForUpdaterRelease(ctx context.Context, target string) error {
	for {
		client := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", s.socketPath)
			}},
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/status", nil)
		resp, err := client.Do(req)
		if err == nil {
			var status struct {
				ProtocolVersion int      `json:"protocol_version"`
				UpdaterVersion  string   `json:"updater_version"`
				Capabilities    []string `json:"capabilities"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&status)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil &&
				status.ProtocolVersion >= updaterProtocolVersion && status.UpdaterVersion == target {
				for _, capability := range status.Capabilities {
					if capability == controlPlaneCapability {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("matching updater %s did not become ready: %w", target, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func (s *server) requireServiceImageRef(service, want string) error {
	if s.imageRef == nil {
		return errors.New("image reference inspection is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	got, err := s.imageRef(ctx, service)
	cancel()
	if err != nil {
		return fmt.Errorf("inspect %s image reference: %w", service, err)
	}
	if got != want {
		return fmt.Errorf("service %s runs %s, want %s", service, got, want)
	}
	return nil
}

func (s *server) rollbackControlPlane(base State, cause error, stopHeartbeat func()) {
	base.State = "restoring"
	base.Message = "paired update failed; restoring previous updater and Core: " + cause.Error()
	base.UpdatedAt = s.nowTime()
	s.writeState(base)
	cleanup, preflightErr := s.prepareControlPlaneTagPins()
	if preflightErr == nil {
		defer cleanup()
	}
	var failures []string
	if preflightErr != nil {
		failures = append(failures, "rollback preflight: "+preflightErr.Error())
	} else {
		updaterSpec, _ := s.componentSpec("updater")
		coreSpec, _ := s.componentSpec("core")
		if err := s.restorePreviousComponentImage(base.PreviousImages["updater"], updaterSpec); err != nil {
			failures = append(failures, "updater: "+err.Error())
		}
		if err := s.restorePreviousComponentImage(base.PreviousImages["core"], coreSpec); err != nil {
			failures = append(failures, "Core: "+err.Error())
		}
	}
	base.State = "failed"
	base.UpdatedAt = s.nowTime()
	if len(failures) == 0 {
		base.Message = "paired update failed; previous Core/updater pair restored: " + cause.Error()
	} else {
		base.Message = "paired update failed and pair rollback was incomplete: " + cause.Error() + "; " + strings.Join(failures, "; ")
	}
	stopHeartbeat()
	s.writeState(base)
}

func (s *server) controlPlaneSpecs() ([]componentSpec, error) {
	core, err := s.componentSpec("core")
	if err != nil {
		return nil, err
	}
	updater, err := s.componentSpec("updater")
	if err != nil {
		return nil, err
	}
	return []componentSpec{core, updater}, nil
}

func (s *server) prepareControlPlaneDigestPins(release controlPlaneRelease) (func(), error) {
	if err := release.validate(); err != nil {
		return func() {}, err
	}
	return s.prepareControlPlaneOverride(map[string]string{
		s.mainServiceName:  release.coreRef(),
		updaterServiceName: release.updaterRef(),
	})
}

func (s *server) validateControlPlaneDigestPins(release controlPlaneRelease) error {
	return s.validateControlPlaneRefs(map[string]string{
		s.mainServiceName:  release.coreRef(),
		updaterServiceName: release.updaterRef(),
	})
}

func (s *server) prepareControlPlaneTagPins() (func(), error) {
	specs, err := s.controlPlaneSpecs()
	if err != nil {
		return func() {}, err
	}
	images := make(map[string]string, len(specs))
	for _, spec := range specs {
		images[spec.service] = spec.image + ":${" + spec.tagVariable + ":-latest}"
	}
	return s.prepareControlPlaneOverride(images)
}

func (s *server) prepareControlPlaneOverride(images map[string]string) (func(), error) {
	if s.updateOverrideFile != "" {
		return func() {}, errors.New("a control-plane image override is already active")
	}
	type imageService struct {
		Image string `yaml:"image"`
	}
	doc := struct {
		Services map[string]imageService `yaml:"services"`
	}{Services: make(map[string]imageService, len(images))}
	for service, image := range images {
		if _, ok, err := serviceImageFromComposeFiles(s.composeFiles(), service); err != nil {
			return func() {}, err
		} else if !ok {
			return func() {}, fmt.Errorf("service %q has no image", service)
		}
		doc.Services[service] = imageService{Image: image}
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return func() {}, err
	}
	f, err := os.CreateTemp("", "ftw-control-plane-update-*.yml")
	if err != nil {
		return func() {}, err
	}
	path := f.Name()
	remove := func() {
		_ = f.Close()
		_ = os.Remove(path)
	}
	if _, err := f.Write(data); err != nil {
		remove()
		return func() {}, err
	}
	if err := f.Sync(); err != nil {
		remove()
		return func() {}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return func() {}, err
	}
	s.updateOverrideFile = path
	return func() {
		s.updateOverrideFile = ""
		_ = os.Remove(path)
	}, nil
}

func (s *server) validateControlPlaneRefs(images map[string]string) error {
	for service, want := range images {
		got, ok, err := serviceImageFromComposeFiles(s.composeFiles(), service)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("service %q has no image", service)
		}
		if got != want {
			return fmt.Errorf("service %q image %q does not match verified ref %q", service, got, want)
		}
	}
	return nil
}

func (s *server) currentServiceImageRef(ctx context.Context, service string) (string, error) {
	containerID, err := s.serviceContainerID(ctx, service)
	if err != nil {
		return "", err
	}
	out, err := dockerOutput(ctx, "inspect", "--format", "{{.Config.Image}}", containerID)
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(out)
	if ref == "" {
		return "", errors.New("running container has no image reference")
	}
	return ref, nil
}
