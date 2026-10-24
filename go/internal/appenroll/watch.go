package appenroll

import (
	"encoding/base64"
	"errors"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

// CheckWatch decides whether a finished exchange handshake may become a
// watch's session, and changes nothing until commit runs. A watch the owner's
// phone added by its key (AddWatch) is admitted; anything else, a phone's key
// among them, answers as a stranger: phones have their stream. There is no
// code: a watch reaches the box through FTW Cloud, which admits only what
// the roster lists.
//
// The exchange commits once the session has proved itself with its first
// transport message, so a message 1 replayed by whoever carries it stamps
// nothing and writes nothing. commit returns the grant as it stands then, and
// fails if the watch or its phone was removed meanwhile.
func (i *Identity) CheckWatch(watchStatic []byte) (Grant, func() (Grant, error), error) {
	key := base64.RawURLEncoding.EncodeToString(watchStatic)
	i.mu.Lock()
	meta := i.authorised[key]
	if meta == nil || meta.role != apiauth.RoleWatch || i.authorised[meta.parent] == nil {
		i.mu.Unlock()
		return Grant{}, nil, ErrNoPairing
	}
	grant := i.grantLocked(key, meta)
	i.mu.Unlock()
	return grant, func() (Grant, error) {
		i.mu.Lock()
		meta := i.authorised[key]
		if meta == nil || meta.role != apiauth.RoleWatch || i.authorised[meta.parent] == nil {
			i.mu.Unlock()
			return Grant{}, ErrNoPairing
		}
		meta.lastSeenMs = i.now().UnixMilli()
		grant := i.grantLocked(key, meta)
		i.mu.Unlock()
		// A stamp that could not be persisted must not block a session.
		_ = i.save()
		return grant, nil
	}, nil
}

// HasWatch reports whether a device id names a watch whose phone is still
// paired. It costs a map walk and no cryptography, so the exchange asks it
// before a handshake's Diffie-Hellman.
func (i *Identity) HasWatch(deviceID string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, meta := i.findLocked(deviceID)
	return meta != nil && meta.role == apiauth.RoleWatch && i.authorised[meta.parent] != nil
}

// WatchKeyBytes is the length of a watch's Noise static key.
const WatchKeyBytes = 32

// ErrWatchKey is a watch key that is not one: the wrong length, or a key the
// box already knows as a phone or an agent.
var ErrWatchKey = errors.New("appenroll: that is not a watch key this box can add")

// AddWatch stores a watch's key with role watch and the owner phone named by
// parentID as its parent, with no code (ADR 0009, decision 4, as FTW Cloud
// carries it). The phone vouches for the key over its own session, with a
// passkey step-up the API checks; the watch proves it holds the key in every
// handshake after. No code crosses FTW Cloud, and the next roster lists the
// watch, so its first request is admitted like any other.
//
// Adding the same watch again changes nothing. The parent must be an owner
// now, as for a watch code.
func (i *Identity) AddWatch(parentID string, watchStatic []byte) (Grant, error) {
	if len(watchStatic) != WatchKeyBytes {
		return Grant{}, ErrWatchKey
	}
	key := base64.RawURLEncoding.EncodeToString(watchStatic)

	i.mu.Lock()
	parentKey, parent := i.findLocked(parentID)
	switch {
	case parent == nil:
		i.mu.Unlock()
		return Grant{}, ErrUnknownDevice
	case parent.role != apiauth.RoleOwner:
		i.mu.Unlock()
		return Grant{}, ErrWatchNeedsOwner
	}
	if meta := i.authorised[key]; meta != nil {
		if meta.role != apiauth.RoleWatch || meta.parent != parentKey {
			i.mu.Unlock()
			return Grant{}, ErrWatchKey
		}
		grant := i.grantLocked(key, meta)
		i.mu.Unlock()
		return grant, nil
	}
	now := i.now().UnixMilli()
	meta := &appMeta{addedAtMs: now, role: apiauth.RoleWatch, epoch: firstEpoch, parent: parentKey}
	i.authorised[key] = meta
	grant := i.grantLocked(key, meta)
	i.mu.Unlock()

	err := i.save()
	i.changed()
	return grant, err
}
