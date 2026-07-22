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
	"time"

	"gopkg.in/yaml.v3"
)

func controlPlaneEnv(target string) []string {
	return []string{"FTW_IMAGE_TAG=" + target, "FTW_UPDATER_IMAGE_TAG=" + target}
}

func (s *server) startControlPlaneHeartbeat(target string) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				st := s.readState()
				if st.Action == "update" && st.Component == "core" && st.Target == target &&
					(st.State == "pulling" || st.State == "transacting") {
					st.UpdatedAt = time.Now()
					s.writeState(st)
				}
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

// runControlPlaneUpdate captures both rollback images and pulls the matched
// pair. A detached helper then owns replacement and rollback while the updater
// service replaces itself.
func (s *server) runControlPlaneUpdate(target string, startedAt time.Time) {
	stopHeartbeat := s.startControlPlaneHeartbeat(target)
	defer stopHeartbeat()
	base := State{State: "pulling", Action: "update", Component: "core", Target: target, StartedAt: startedAt, UpdatedAt: time.Now()}
	s.writeState(base)

	cleanup, err := s.prepareControlPlaneImagePins()
	if err != nil {
		s.failControlPlanePreflight(base, err)
		return
	}
	defer cleanup()
	if err := s.validateControlPlaneImagePins(); err != nil {
		s.failControlPlanePreflight(base, err)
		return
	}
	specs, err := s.controlPlaneSpecs()
	if err != nil {
		base.State, base.Message, base.UpdatedAt = "failed", err.Error(), time.Now()
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
			base.UpdatedAt = time.Now()
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
			base.State, base.Message, base.UpdatedAt = "failed", "paired pull failed: "+pullErr.Error(), time.Now()
			s.writeState(base)
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	updaterContainer, err := s.containerID(ctx, updaterServiceName)
	cancel()
	if err != nil {
		base.State, base.Message, base.UpdatedAt = "failed", "cannot locate updater container: "+err.Error(), time.Now()
		s.writeState(base)
		return
	}
	base.State = "transacting"
	base.Message = "paired images pulled; detached helper owns the transaction"
	base.UpdatedAt = time.Now()
	s.writeState(base)
	if s.launchTransaction != nil {
		if err := s.launchTransaction(target, previous, startedAt); err != nil {
			base.State, base.Message, base.UpdatedAt = "failed", "cannot start transaction helper: "+err.Error(), time.Now()
			s.writeState(base)
		}
		return
	}

	args := []string{
		"run", "--rm", "--detach", "--name", fmt.Sprintf("ftw-control-plane-%d", time.Now().UnixNano()),
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
	args = append(args,
		previous["updater"],
		"-control-plane-transaction", target,
		"-previous-core-image", previous["core"],
		"-previous-updater-image", previous["updater"],
		"-transaction-started-at", startedAt.Format(time.RFC3339Nano),
	)
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 2*time.Minute)
	err = s.runner(launchCtx, nil, args...)
	cancelLaunch()
	if err != nil {
		base.State, base.Message, base.UpdatedAt = "failed", "cannot start transaction helper: "+err.Error(), time.Now()
		s.writeState(base)
	}
}

func (s *server) failControlPlanePreflight(base State, err error) {
	base.State = "failed"
	base.Message = "compose preflight failed: " + err.Error()
	base.UpdatedAt = time.Now()
	s.writeState(base)
}

func (s *server) runControlPlaneTransaction(target string, previous map[string]string, startedAt time.Time) {
	stopHeartbeat := s.startControlPlaneHeartbeat(target)
	defer stopHeartbeat()
	base := State{
		State: "transacting", Action: "update", Component: "core", Target: target,
		StartedAt: startedAt, UpdatedAt: time.Now(), PreviousImageID: previous["core"], PreviousImages: previous,
	}
	fail := func(err error) { s.rollbackControlPlane(base, err) }
	if !isImmutableImageTag(target) || previous["core"] == "" || previous["updater"] == "" {
		fail(errors.New("invalid paired transaction parameters"))
		return
	}
	cleanup, err := s.prepareControlPlaneImagePins()
	if err != nil {
		fail(fmt.Errorf("compose preflight: %w", err))
		return
	}
	defer cleanup()
	if err := s.validateControlPlaneImagePins(); err != nil {
		fail(fmt.Errorf("compose preflight: %w", err))
		return
	}
	env := controlPlaneEnv(target)

	base.Message = "replacing updater with matching release"
	base.UpdatedAt = time.Now()
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
		err = s.updaterReady(compatCtx, target)
	} else {
		err = s.waitForUpdaterRelease(compatCtx, target)
	}
	cancelCompat()
	if err != nil {
		fail(err)
		return
	}
	if err := s.requireServiceImageRef(updaterServiceName, canonicalUpdaterImage+":"+target); err != nil {
		fail(err)
		return
	}

	base.Message = "matching updater ready; replacing Core"
	base.UpdatedAt = time.Now()
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
	if err := s.requireServiceImageRef(s.mainServiceName, canonicalMainImage+":"+target); err != nil {
		fail(err)
		return
	}
	if err := s.requireServiceImageRef(updaterServiceName, canonicalUpdaterImage+":"+target); err != nil {
		fail(err)
		return
	}
	base.State = "done"
	base.Message = "Core and updater committed as one release pair"
	base.UpdatedAt = time.Now()
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

func (s *server) rollbackControlPlane(base State, cause error) {
	base.State = "restoring"
	base.Message = "paired update failed; restoring previous updater and Core: " + cause.Error()
	base.UpdatedAt = time.Now()
	s.writeState(base)
	cleanup, preflightErr := s.prepareControlPlaneImagePins()
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
	base.UpdatedAt = time.Now()
	if len(failures) == 0 {
		base.Message = "paired update failed; previous Core/updater pair restored: " + cause.Error()
	} else {
		base.Message = "paired update failed and pair rollback was incomplete: " + cause.Error() + "; " + strings.Join(failures, "; ")
	}
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

func (s *server) prepareControlPlaneImagePins() (func(), error) {
	specs, err := s.controlPlaneSpecs()
	if err != nil {
		return func() {}, err
	}
	type imageService struct {
		Image string `yaml:"image"`
	}
	doc := struct {
		Services map[string]imageService `yaml:"services"`
	}{Services: make(map[string]imageService)}
	for _, spec := range specs {
		image, ok, err := serviceImageFromComposeFiles(s.composeFiles(), spec.service)
		if err != nil {
			return func() {}, err
		}
		if !ok {
			return func() {}, fmt.Errorf("service %q has no image", spec.service)
		}
		if !strings.Contains(image, spec.tagVariable) {
			doc.Services[spec.service] = imageService{Image: spec.image + ":${" + spec.tagVariable + ":-latest}"}
		}
	}
	if len(doc.Services) == 0 {
		return func() {}, nil
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

func (s *server) validateControlPlaneImagePins() error {
	specs, err := s.controlPlaneSpecs()
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if err := s.validateComponentImagePin(spec); err != nil {
			return err
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
