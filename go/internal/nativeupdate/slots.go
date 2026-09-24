package nativeupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"time"
)

const stateFile = "slots.json"

var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-beta\.[0-9]+)?$`)

// Slots is the one durable source of truth for the installed releases.
// All transitions replace this file in one rename and sync its directory.
type Slots struct {
	Current    string `json:"current"`
	Previous   string `json:"previous,omitempty"`
	Next       string `json:"next,omitempty"`
	Trial      string `json:"trial,omitempty"`
	LastFailed string `json:"last_failed,omitempty"`
	// Probation watches the current release for a while after it
	// committed; see probation.go.
	Probation *Probation `json:"probation,omitempty"`
}

type Manager struct {
	Root string
	Now  func() time.Time // nil uses time.Now
}

func ValidTag(tag string) bool { return releaseTag.MatchString(tag) }

func (m Manager) ReleaseDir(tag string) (string, error) {
	if !ValidTag(tag) {
		return "", fmt.Errorf("invalid release tag %q", tag)
	}
	path := filepath.Join(m.Root, "releases", tag)
	if err := validateReleasePath(path, tag, runtime.GOARCH); err != nil {
		return "", err
	}
	return path, nil
}

func validateReleasePath(path, tag, arch string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("release %s is not a directory", tag)
	}
	for _, name := range []string{"ftw", "web/index.html", "drivers/BUNDLED_SOURCE.json", "optimizer/native/bundle/manifest.json"} {
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			item, err := os.Lstat(filepath.Join(path, parent))
			if err != nil || !item.IsDir() {
				return fmt.Errorf("release %s has unsafe directory %s", tag, parent)
			}
		}
		item, err := os.Lstat(filepath.Join(path, name))
		if err != nil {
			return fmt.Errorf("release %s is incomplete: %s: %w", tag, name, err)
		}
		if !item.Mode().IsRegular() {
			return fmt.Errorf("release %s has unsafe file %s", tag, name)
		}
		if name == "ftw" && item.Mode()&0o111 == 0 {
			return fmt.Errorf("release %s has no executable ftw", tag)
		}
	}
	if err := checkReceipt(path, tag, arch); err != nil {
		return err
	}
	return nil
}

func (m Manager) Read() (Slots, error) {
	var state Slots
	err := m.locked(func() error {
		var readErr error
		state, readErr = m.readLocked()
		return readErr
	})
	return state, err
}

func (m Manager) Init(tag string) error {
	if _, err := m.ReleaseDir(tag); err != nil {
		return err
	}
	return m.locked(func() error {
		state, err := m.readLocked()
		if err == nil {
			if state.Current == tag {
				return nil
			}
			return fmt.Errorf("already initialized at %s", state.Current)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return m.writeLocked(Slots{Current: tag})
	})
}

func (m Manager) Prepare(tag string) error {
	candidateDir, err := m.ReleaseDir(tag)
	if err != nil {
		return err
	}
	return m.change(func(state *Slots) error {
		if state.Trial != "" || state.Next != "" {
			return errors.New("another release is pending")
		}
		if tag == state.Current {
			return errors.New("release is already current")
		}
		currentDir, err := m.ReleaseDir(state.Current)
		if err != nil {
			return err
		}
		current, err := readReceipt(currentDir, state.Current, runtime.GOARCH)
		if err != nil {
			return err
		}
		candidate, err := readReceipt(candidateDir, tag, runtime.GOARCH)
		if err != nil {
			return err
		}
		if current.StateSchema != candidate.StateSchema {
			return fmt.Errorf("state schema changes from %d to %d; automatic rollback would be unsafe", current.StateSchema, candidate.StateSchema)
		}
		state.Next = tag
		state.LastFailed = ""
		return nil
	})
}

// Select runs at every service start. An uncommitted trial means the trial
// process exited or the host rebooted; it always falls back to current. A
// release on probation that keeps stopping without a clean shutdown falls
// back to the previous release.
func (m Manager) Select() (path, tag string, trial bool, err error) {
	err = m.locked(func() error {
		state, readErr := m.readLocked()
		if readErr != nil {
			return readErr
		}
		cleanTag := m.takeCleanExit()
		if state.Trial != "" {
			state.LastFailed = state.Trial
			state.Trial = ""
			state.Next = ""
			if writeErr := m.writeLocked(state); writeErr != nil {
				return writeErr
			}
		} else if state.Next != "" {
			if _, releaseErr := m.ReleaseDir(state.Next); releaseErr != nil {
				state.LastFailed = state.Next
				state.Next = ""
				if writeErr := m.writeLocked(state); writeErr != nil {
					return writeErr
				}
			} else {
				state.Trial = state.Next
				state.Next = ""
				if writeErr := m.writeLocked(state); writeErr != nil {
					return writeErr
				}
				trial = true
			}
		} else if m.watchProbation(&state, cleanTag) {
			if writeErr := m.writeLocked(state); writeErr != nil {
				return writeErr
			}
		}
		tag = state.Current
		if trial {
			tag = state.Trial
		}
		path, readErr = m.ReleaseDir(tag)
		return readErr
	})
	return
}

func (m Manager) Commit(tag string) error {
	return m.change(func(state *Slots) error {
		if state.Trial != tag || tag == state.Current {
			return fmt.Errorf("release %s is not the active trial", tag)
		}
		if _, err := m.ReleaseDir(tag); err != nil {
			return err
		}
		state.Previous = state.Current
		state.Current = tag
		state.Trial = ""
		// A rollback that commits does not clear a failed release: an
		// unattended update would otherwise install it again.
		if state.LastFailed == tag {
			state.LastFailed = ""
		}
		state.Probation = &Probation{Tag: tag, Until: m.now().Add(ProbationWindow)}
		return nil
	})
}

func (m Manager) CancelPrepared(tag string) error {
	return m.change(func(state *Slots) error {
		if state.Next != tag || state.Trial != "" {
			return fmt.Errorf("release %s is not waiting to start", tag)
		}
		state.Next = ""
		state.LastFailed = tag
		return nil
	})
}

func (m Manager) FailTrial(tag string) error {
	return m.change(func(state *Slots) error {
		if state.Trial != tag {
			return fmt.Errorf("release %s is not the active trial", tag)
		}
		state.Trial = ""
		state.LastFailed = tag
		return nil
	})
}

func (m Manager) PrepareRollback() (string, error) {
	var previous string
	err := m.locked(func() error {
		state, readErr := m.readLocked()
		if readErr != nil {
			return readErr
		}
		previous, readErr = m.rollbackCandidateLocked(state)
		if readErr != nil {
			return readErr
		}
		state.Next = previous
		return m.writeLocked(state)
	})
	return previous, err
}

// RollbackCandidate reports a safe binary-only rollback without changing
// slots. The UI can then hide a previous release that needs data restore.
func (m Manager) RollbackCandidate() (string, error) {
	var previous string
	err := m.locked(func() error {
		state, readErr := m.readLocked()
		if readErr != nil {
			return readErr
		}
		previous, readErr = m.rollbackCandidateLocked(state)
		return readErr
	})
	return previous, err
}

func (m Manager) rollbackCandidateLocked(state Slots) (string, error) {
	if state.Previous == "" || state.Next != "" || state.Trial != "" {
		return "", errors.New("no previous release is available for rollback")
	}
	previousDir, err := m.ReleaseDir(state.Previous)
	if err != nil {
		return "", err
	}
	currentDir, err := m.ReleaseDir(state.Current)
	if err != nil {
		return "", err
	}
	previousReceipt, err := readReceipt(previousDir, state.Previous, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	currentReceipt, err := readReceipt(currentDir, state.Current, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	if currentReceipt.StateSchema != previousReceipt.StateSchema {
		return "", errors.New("state schema changed; use a verified full backup for rollback")
	}
	return state.Previous, nil
}

// Prune keeps the current, previous, pending and last failed releases. It
// never removes an unknown entry or a release while a trial is active.
func (m Manager) Prune() ([]string, error) {
	var removed []string
	err := m.locked(func() error {
		state, err := m.readLocked()
		if err != nil {
			return err
		}
		if state.Trial != "" {
			return errors.New("cannot prune during a trial")
		}
		if _, err := m.ReleaseDir(state.Current); err != nil {
			return fmt.Errorf("current release is not healthy: %w", err)
		}
		if state.Previous != "" {
			if _, err := m.ReleaseDir(state.Previous); err != nil {
				return fmt.Errorf("previous release is not healthy: %w", err)
			}
		}
		keep := map[string]bool{
			state.Current: true, state.Previous: true,
			state.Next: true, state.LastFailed: true,
		}
		releases := filepath.Join(m.Root, "releases")
		entries, err := os.ReadDir(releases)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || !ValidTag(name) || keep[name] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(releases, name)); err != nil {
				return err
			}
			removed = append(removed, name)
		}
		return syncDir(releases)
	})
	return removed, err
}

func (m Manager) change(update func(*Slots) error) error {
	return m.locked(func() error {
		state, err := m.readLocked()
		if err != nil {
			return err
		}
		if err := update(&state); err != nil {
			return err
		}
		return m.writeLocked(state)
	})
}

func (m Manager) locked(fn func() error) error {
	lock, err := os.OpenFile(filepath.Join(m.Root, ".slots.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func (m Manager) readLocked() (Slots, error) {
	var state Slots
	data, err := os.ReadFile(filepath.Join(m.Root, stateFile))
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if !ValidTag(state.Current) {
		return state, errors.New("invalid current release in slots file")
	}
	for _, tag := range []string{state.Previous, state.Next, state.Trial, state.LastFailed} {
		if tag != "" && !ValidTag(tag) {
			return state, errors.New("invalid release tag in slots file")
		}
	}
	return state, nil
}

func (m Manager) writeLocked(state Slots) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(m.Root, ".slots-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(m.Root, stateFile)); err != nil {
		return err
	}
	dir, err := os.Open(m.Root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
