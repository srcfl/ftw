package nativeupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A release that has just committed is on probation. A Core that keeps
// stopping without a clean shutdown in that time is replaced by the release
// before it, so an update that crashes after it became ready still falls
// back without operator action (ADR 0007, decision 2).
const (
	ProbationWindow = time.Hour
	// crashLimit starts without a clean shutdown within crashWindow count as
	// a crash loop. One power cut or one crash is not enough.
	crashLimit  = 3
	crashWindow = 10 * time.Minute

	cleanExitFile = ".clean-exit"
)

// Probation watches the current release after it committed.
type Probation struct {
	Tag     string      `json:"tag"`
	Until   time.Time   `json:"until"`
	Crashes []time.Time `json:"crashes,omitempty"`
}

func (m Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// MarkCleanExit records that the running Core is stopping on purpose: a
// service stop, a restart for settings or an update. The launcher then
// does not count the next start as a crash.
func (m Manager) MarkCleanExit(tag string) error {
	path := filepath.Join(m.Root, cleanExitFile)
	tmp, err := os.CreateTemp(m.Root, ".clean-exit-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(tag + "\n"); err != nil {
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(m.Root)
}

// takeCleanExit reads and removes the clean-exit mark, so it speaks for
// exactly one stop.
func (m Manager) takeCleanExit() string {
	path := filepath.Join(m.Root, cleanExitFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	_ = os.Remove(path)
	return strings.TrimSpace(string(data))
}

// watchProbation updates the probation of the current release for one start
// and reports whether it falls back to the previous release.
func (m Manager) watchProbation(state *Slots, cleanTag string) (changed bool) {
	p := state.Probation
	if p == nil {
		return false
	}
	now := m.now()
	if p.Tag != state.Current || now.After(p.Until) {
		state.Probation = nil
		return true
	}
	if cleanTag == state.Current {
		return false
	}
	recent := p.Crashes[:0]
	for _, at := range p.Crashes {
		if now.Sub(at) < crashWindow {
			recent = append(recent, at)
		}
	}
	p.Crashes = append(recent, now)
	if len(p.Crashes) < crashLimit {
		return true
	}
	if _, err := m.rollbackCandidateLocked(*state); err != nil {
		// No release that reads this data: keep trying the current one.
		return true
	}
	state.LastFailed = state.Current
	state.Current = state.Previous
	state.Previous = ""
	state.Probation = nil
	return true
}

// errNotOwner stops a launcher run by another user, usually root, from
// writing slot files the service account can no longer read.
var errNotOwner = errors.New("run the launcher as the owner of the install root, for example: sudo -u ftw")

// CheckOwner refuses a root process in an install root owned by the service
// account. Files it wrote would be unreadable to the service.
func (m Manager) CheckOwner() error {
	info, err := os.Stat(m.Root)
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 && st.Uid != 0 {
		return errNotOwner
	}
	return nil
}
