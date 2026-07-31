package nova

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
)

const (
	maxIdentityKeyBytes       = 16 << 10
	identityBarrierTimeout    = 5 * time.Second
	identityBarrierPollPeriod = 5 * time.Millisecond
)

var ErrIdentityCreationBlocked = errors.New("nova: identity creation is blocked by reserved state")

// Identity is the ES256 keypair this FTW instance uses to
// authenticate with Nova. The private key signs MQTT auth JWTs (see
// SignJWT) and claim-flow proof messages (see SignClaimMessage).
type Identity struct {
	priv *ecdsa.PrivateKey
}

// LoadOrCreateIdentity reads an ES256 private key from path, or creates one
// without replacing a concurrent winner. The file format is a standard
// PEM-encoded EC private key matching Nova's existing key layout.
//
// Secure persistence needs a local file system that exposes stable Unix owner
// and link metadata, supports same-directory hard links, and implements file
// and directory sync. Other file systems fail closed; there is no copy or
// rename fallback. The enclosing directory is created with 0700 only when its
// trusted parent already exists. The key file is created with 0600.
func LoadOrCreateIdentity(path string) (*Identity, error) {
	policy, err := currentIdentityFilePolicy()
	if err != nil {
		return nil, err
	}
	return loadOrCreateIdentity(path, policy, defaultIdentityFileOps())
}

// LoadExistingIdentity reads and verifies an existing key without changing the
// file system. It never creates a directory or key and never cleans install
// state. Missing path components remain fs.ErrNotExist-compatible.
func LoadExistingIdentity(path string) (*Identity, error) {
	policy, err := currentIdentityFilePolicy()
	if err != nil {
		return nil, err
	}
	return loadExistingIdentity(path, policy, existingIdentityHooks{})
}

// LoadOrCreateIdentityGuarded loads an existing key or creates one only while
// reserved files remain absent under the same pinned directory descriptor.
// lockDirectory must exclude every writer that can install a reserved file.
func LoadOrCreateIdentityGuarded(
	path string,
	reservedNames []string,
	lockDirectory func(*os.File) error,
) (*Identity, error) {
	policy, err := currentIdentityFilePolicy()
	if err != nil {
		return nil, err
	}
	return loadOrCreateIdentityGuarded(
		path, reservedNames, lockDirectory, policy, defaultIdentityFileOps(),
		guardedIdentityHooks{},
	)
}

type existingIdentityHooks struct {
	afterParentOpen    func() error
	afterDirectoryOpen func() error
	afterKeyOpen       func() error
}

type guardedIdentityHooks struct {
	afterAbsence func() error
}

func loadExistingIdentity(
	path string,
	policy identityFilePolicy,
	hooks existingIdentityHooks,
) (*Identity, error) {
	if path == "" {
		return nil, errors.New("nova: key path is empty")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("nova: resolve key path: %w", err)
	}
	keyName := filepath.Base(absPath)
	if keyName == "." || keyName == string(filepath.Separator) ||
		!fs.ValidPath(filepath.ToSlash(keyName)) {
		return nil, errors.New("nova: key path has no file name")
	}
	dirPath := filepath.Dir(absPath)
	parentPath := filepath.Dir(dirPath)
	var dir *identityDirectory
	if parentPath == dirPath {
		dir, err = openIdentityDirectoryPath(dirPath, policy.directoryOwnerUID, false)
	} else {
		parent, parentErr := openIdentityDirectoryPath(
			parentPath, policy.directoryOwnerUID, true,
		)
		if parentErr != nil {
			return nil, fmt.Errorf("nova: open existing key directory parent: %w", parentErr)
		}
		if hooks.afterParentOpen != nil {
			if hookErr := hooks.afterParentOpen(); hookErr != nil {
				parent.close()
				return nil, hookErr
			}
		}
		dir, err = openIdentityDirectoryEntry(
			parent, filepath.Base(dirPath), dirPath, policy.directoryOwnerUID,
		)
		if err != nil {
			parent.close()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("nova: open existing key directory: %w", err)
	}
	defer dir.close()
	if hooks.afterDirectoryOpen != nil {
		if err := hooks.afterDirectoryOpen(); err != nil {
			return nil, err
		}
	}
	identity, err := loadIdentityWithHook(dir, keyName, policy, hooks.afterKeyOpen)
	if err != nil {
		return nil, err
	}
	if err := dir.revalidate(policy); err != nil {
		return nil, err
	}
	return identity, nil
}

type identityFilePolicy struct {
	directoryOwnerUID uint64
	fileOwnerUID      uint64
	barrierTimeout    time.Duration
	barrierPollPeriod time.Duration
}

type identityFileOps struct {
	link          func(*os.Root, string, string) error
	remove        func(*os.Root, string) error
	syncDirectory func(*os.File) error
	stat          func(*os.File) (os.FileInfo, error)
	beforeLink    func() error
	now           func() time.Time
	sleep         func(time.Duration)
}

type identityDirectory struct {
	path      string
	file      *os.File
	root      *os.Root
	parent    *identityDirectory
	entryName string
}

func currentIdentityFilePolicy() (identityFilePolicy, error) {
	uid := os.Geteuid()
	if uid < 0 {
		return identityFilePolicy{}, errors.New("nova: key storage requires file owner metadata")
	}
	return identityFilePolicy{
		directoryOwnerUID: uint64(uid),
		fileOwnerUID:      uint64(uid),
		barrierTimeout:    identityBarrierTimeout,
		barrierPollPeriod: identityBarrierPollPeriod,
	}, nil
}

func defaultIdentityFileOps() identityFileOps {
	return identityFileOps{
		link: func(root *os.Root, oldName, newName string) error {
			return root.Link(oldName, newName)
		},
		remove: func(root *os.Root, name string) error {
			return root.Remove(name)
		},
		syncDirectory: func(dir *os.File) error {
			return dir.Sync()
		},
		stat: func(file *os.File) (os.FileInfo, error) {
			return file.Stat()
		},
		now: time.Now, sleep: time.Sleep,
	}
}

func loadOrCreateIdentity(path string, policy identityFilePolicy, ops identityFileOps) (*Identity, error) {
	if path == "" {
		return nil, errors.New("nova: key path is empty")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("nova: resolve key path: %w", err)
	}
	keyName := filepath.Base(absPath)
	if keyName == "." || keyName == string(filepath.Separator) || !fs.ValidPath(filepath.ToSlash(keyName)) {
		return nil, errors.New("nova: key path has no file name")
	}
	dir, created, err := openOrCreateIdentityDirectory(filepath.Dir(absPath), policy, ops)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	return loadOrCreateIdentityInDirectory(
		dir, created, keyName, policy, ops, nil, false,
	)
}

func loadOrCreateIdentityGuarded(
	path string,
	reservedNames []string,
	lockDirectory func(*os.File) error,
	policy identityFilePolicy,
	ops identityFileOps,
	hooks guardedIdentityHooks,
) (*Identity, error) {
	if lockDirectory == nil {
		return nil, errors.New("nova: identity directory lock is missing")
	}
	if path == "" {
		return nil, errors.New("nova: key path is empty")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("nova: resolve key path: %w", err)
	}
	keyName := filepath.Base(absPath)
	if keyName == "." || keyName == string(filepath.Separator) ||
		!fs.ValidPath(filepath.ToSlash(keyName)) {
		return nil, errors.New("nova: key path has no file name")
	}
	names, err := validateReservedIdentityNames(keyName, reservedNames)
	if err != nil {
		return nil, err
	}
	dir, created, err := openOrCreateIdentityDirectory(filepath.Dir(absPath), policy, ops)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	if err := lockDirectory(dir.file); err != nil {
		return nil, fmt.Errorf("nova: lock identity directory: %w", err)
	}
	if err := ensureReservedIdentityFilesAbsent(dir, names, policy); err != nil {
		return nil, err
	}
	beforeInstall := func() error {
		return ensureReservedIdentityFilesAbsent(dir, names, policy)
	}
	guardedOps := ops
	priorBeforeLink := guardedOps.beforeLink
	guardedOps.beforeLink = func() error {
		if priorBeforeLink != nil {
			if err := priorBeforeLink(); err != nil {
				return err
			}
		}
		if hooks.afterAbsence != nil {
			if err := hooks.afterAbsence(); err != nil {
				return err
			}
		}
		return ensureReservedIdentityFilesAbsent(dir, names, policy)
	}
	return loadOrCreateIdentityInDirectory(
		dir, created, keyName, policy, guardedOps, beforeInstall, true,
	)
}

func loadOrCreateIdentityInDirectory(
	dir *identityDirectory,
	created bool,
	keyName string,
	policy identityFilePolicy,
	ops identityFileOps,
	beforeInstall func() error,
	rollbackPostLinkMismatch bool,
) (*Identity, error) {
	if info, err := dir.root.Lstat(keyName); err == nil {
		links, err := validateIdentityFileInfo(info, policy.fileOwnerUID, true)
		if err != nil {
			return nil, err
		}
		if links != 1 {
			if err := waitForDurableIdentity(dir, keyName, policy, ops); err != nil {
				return nil, err
			}
		} else if err := ops.syncDirectory(dir.file); err != nil {
			return nil, fmt.Errorf("nova: sync existing key: %w", err)
		}
		identity, err := loadIdentity(dir, keyName, policy)
		if err != nil {
			return nil, err
		}
		if err := dir.revalidate(policy); err != nil {
			return nil, err
		}
		return identity, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("nova: lstat key: %w", err)
	}

	// A retry must repeat a failed creation barrier before it writes a key.
	if !created && dir.parent != nil {
		if err := ops.syncDirectory(dir.parent.file); err != nil {
			return nil, fmt.Errorf("nova: sync key directory parent: %w", err)
		}
	}
	if err := dir.revalidate(policy); err != nil {
		return nil, err
	}
	if beforeInstall != nil {
		if err := beforeInstall(); err != nil {
			return nil, err
		}
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("nova: generate key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("nova: marshal key: %w", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	installed, err := installIdentityFileNoReplace(dir, keyName, out, 0o600, policy, ops)
	if err != nil {
		writeErr := fmt.Errorf("nova: write key: %w", err)
		if installed && rollbackPostLinkMismatch {
			if revalidateErr := dir.revalidate(policy); revalidateErr != nil {
				rollbackErr := rollbackInstalledIdentity(
					dir, keyName, priv, policy, ops,
				)
				return nil, errors.Join(writeErr, revalidateErr, rollbackErr)
			}
		}
		return nil, writeErr
	}
	if rollbackPostLinkMismatch {
		if err := dir.revalidate(policy); err != nil {
			rollbackErr := rollbackInstalledIdentity(
				dir, keyName, priv, policy, ops,
			)
			return nil, errors.Join(err, rollbackErr)
		}
	}
	persisted, err := loadIdentity(dir, keyName, policy)
	if err != nil {
		return nil, err
	}
	if installed && !publicKeysEqual(priv, persisted.priv) {
		return nil, errors.New("nova: installed key does not match generated key")
	}
	if err := dir.revalidate(policy); err != nil {
		if installed && rollbackPostLinkMismatch {
			rollbackErr := rollbackInstalledIdentity(
				dir, keyName, priv, policy, ops,
			)
			return nil, errors.Join(err, rollbackErr)
		}
		return nil, err
	}
	return persisted, nil
}

func rollbackInstalledIdentity(
	dir *identityDirectory,
	name string,
	expected *ecdsa.PrivateKey,
	policy identityFilePolicy,
	ops identityFileOps,
) error {
	state, err := loadIdentityForRollback(
		dir, name, policy, ops,
	)
	if err != nil {
		return fmt.Errorf("nova: verify linked key before rollback: %w", err)
	}
	defer state.file.Close()
	if !publicKeysEqual(expected, state.identity.priv) {
		return errors.New("nova: linked key changed before rollback")
	}

	expectedLinks := uint64(len(state.tempNames) + 1)
	if err := requireRollbackLinkCount(
		state.file, policy.fileOwnerUID, expectedLinks, ops,
		"before rollback cleanup",
	); err != nil {
		return err
	}

	var cleanupErrs []error
	mutatedDirectory := false
	stopCleanup := false
	for _, tempName := range state.tempNames {
		if err := requireRollbackLinkCount(
			state.file, policy.fileOwnerUID, expectedLinks, ops,
			"before rollback temp unlink",
		); err != nil {
			cleanupErrs = append(cleanupErrs, err)
			stopCleanup = true
			break
		}
		info, statErr := dir.root.Lstat(tempName)
		if statErr != nil {
			cleanupErrs = append(
				cleanupErrs,
				fmt.Errorf("nova: revalidate rollback temp %s: %w", tempName, statErr),
			)
			stopCleanup = true
			break
		}
		if !os.SameFile(state.targetInfo, info) {
			cleanupErrs = append(
				cleanupErrs,
				fmt.Errorf("nova: rollback temp %s changed before removal", tempName),
			)
			stopCleanup = true
			break
		}
		removeErr := ops.remove(dir.root, tempName)
		mutatedDirectory = true
		expectedLinks--
		linkErr := requireRollbackLinkCount(
			state.file, policy.fileOwnerUID, expectedLinks, ops,
			"after rollback temp unlink",
		)
		if removeErr != nil {
			cleanupErrs = append(
				cleanupErrs,
				fmt.Errorf("nova: remove rollback temp %s: %w", tempName, removeErr),
			)
		}
		if linkErr != nil {
			cleanupErrs = append(cleanupErrs, linkErr)
		}
		if removeErr != nil || linkErr != nil {
			stopCleanup = true
			break
		}
	}

	if !stopCleanup {
		if err := requireRollbackLinkCount(
			state.file, policy.fileOwnerUID, expectedLinks, ops,
			"before rollback key unlink",
		); err != nil {
			cleanupErrs = append(cleanupErrs, err)
			stopCleanup = true
		}
	}
	if !stopCleanup {
		currentTarget, targetErr := dir.root.Lstat(name)
		if targetErr != nil {
			cleanupErrs = append(
				cleanupErrs,
				fmt.Errorf("nova: revalidate rollback key: %w", targetErr),
			)
			stopCleanup = true
		} else if !os.SameFile(state.targetInfo, currentTarget) {
			cleanupErrs = append(
				cleanupErrs,
				errors.New("nova: rollback key changed before removal"),
			)
			stopCleanup = true
		}
	}
	if !stopCleanup {
		removeErr := ops.remove(dir.root, name)
		mutatedDirectory = true
		expectedLinks--
		linkErr := requireRollbackLinkCount(
			state.file, policy.fileOwnerUID, expectedLinks, ops,
			"after rollback key unlink",
		)
		if removeErr != nil {
			cleanupErrs = append(
				cleanupErrs,
				fmt.Errorf("nova: remove rollback key: %w", removeErr),
			)
		}
		if linkErr != nil {
			cleanupErrs = append(cleanupErrs, linkErr)
		}
	}

	if mutatedDirectory {
		if syncErr := ops.syncDirectory(dir.file); syncErr != nil {
			cleanupErrs = append(
				cleanupErrs,
				wrapIdentityError("sync rolled back key install", syncErr),
			)
		}
	}
	if len(cleanupErrs) == 0 {
		if !mutatedDirectory || expectedLinks != 0 {
			cleanupErrs = append(
				cleanupErrs,
				errors.New("nova: rollback did not unlink the complete key state"),
			)
		} else if err := requireRollbackLinkCount(
			state.file, policy.fileOwnerUID, 0, ops,
			"after rollback directory sync",
		); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}

	for _, removedName := range append(state.tempNames, name) {
		if _, statErr := dir.root.Lstat(removedName); !errors.Is(statErr, fs.ErrNotExist) {
			if statErr == nil {
				cleanupErrs = append(
					cleanupErrs,
					fmt.Errorf("nova: rolled back entry %s still exists", removedName),
				)
			} else {
				cleanupErrs = append(
					cleanupErrs,
					fmt.Errorf("nova: recheck rolled back entry %s: %w", removedName, statErr),
				)
			}
		}
	}
	return errors.Join(cleanupErrs...)
}

type rollbackIdentityState struct {
	identity   *Identity
	file       *os.File
	targetInfo os.FileInfo
	tempNames  []string
}

func loadIdentityForRollback(
	dir *identityDirectory,
	name string,
	policy identityFilePolicy,
	ops identityFileOps,
) (*rollbackIdentityState, error) {
	lstatInfo, err := dir.root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("nova: lstat rollback key: %w", err)
	}
	lstatLinks, err := validateRollbackIdentityFileInfo(
		lstatInfo, policy.fileOwnerUID, false,
	)
	if err != nil {
		return nil, err
	}

	file, err := openIdentityRootFileNoFollow(dir.root, name)
	if err != nil {
		return nil, fmt.Errorf("nova: open rollback key: %w", err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	fstatInfo, err := ops.stat(file)
	if err != nil {
		return nil, fmt.Errorf("nova: fstat rollback key: %w", err)
	}
	if !os.SameFile(lstatInfo, fstatInfo) {
		return nil, errors.New("nova: rollback key changed while opening")
	}
	fstatLinks, err := validateRollbackIdentityFileInfo(
		fstatInfo, policy.fileOwnerUID, false,
	)
	if err != nil {
		return nil, err
	}
	if fstatLinks != lstatLinks {
		return nil, errors.New("nova: rollback key link count changed while opening")
	}

	b, err := io.ReadAll(io.LimitReader(file, maxIdentityKeyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("nova: read rollback key: %w", err)
	}
	if len(b) > maxIdentityKeyBytes {
		return nil, errors.New("nova: rollback key file is too large")
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf(
			"nova: %s: no PEM block", filepath.Join(dir.path, name),
		)
	}
	priv, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nova: parse rollback key: %w", err)
	}
	if priv.Curve != elliptic.P256() {
		return nil, errors.New("nova: rollback key is not P-256")
	}

	finalInfo, err := dir.root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("nova: revalidate rollback key entry: %w", err)
	}
	if !os.SameFile(fstatInfo, finalInfo) {
		return nil, errors.New("nova: rollback key changed before cleanup")
	}
	finalLinks, err := validateRollbackIdentityFileInfo(
		finalInfo, policy.fileOwnerUID, false,
	)
	if err != nil {
		return nil, err
	}
	if finalLinks != fstatLinks {
		return nil, errors.New("nova: rollback key link count changed before cleanup")
	}

	entries, err := fs.ReadDir(dir.root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("nova: list rollback key directory: %w", err)
	}
	prefix := "." + name + ".tmp-"
	tempNames := make([]string, 0, 1)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, statErr := dir.root.Lstat(entry.Name())
		if statErr != nil {
			return nil, fmt.Errorf(
				"nova: lstat rollback temp %s: %w", entry.Name(), statErr,
			)
		}
		if !os.SameFile(finalInfo, info) {
			return nil, fmt.Errorf(
				"nova: rollback temp %s is not the installed key", entry.Name(),
			)
		}
		tempNames = append(tempNames, entry.Name())
	}
	if uint64(len(tempNames))+1 != finalLinks {
		return nil, fmt.Errorf(
			"nova: rollback key has %d links but %d matching install temps",
			finalLinks, len(tempNames),
		)
	}
	closeFile = false
	return &rollbackIdentityState{
		identity:   &Identity{priv: priv},
		file:       file,
		targetInfo: finalInfo,
		tempNames:  tempNames,
	}, nil
}

func requireRollbackLinkCount(
	file *os.File,
	expectedUID uint64,
	expectedLinks uint64,
	ops identityFileOps,
	phase string,
) error {
	info, err := ops.stat(file)
	if err != nil {
		return fmt.Errorf("nova: fstat rollback key %s: %w", phase, err)
	}
	links, err := validateRollbackIdentityFileInfo(
		info, expectedUID, expectedLinks == 0,
	)
	if err != nil {
		return err
	}
	if links != expectedLinks {
		return fmt.Errorf(
			"nova: rollback key has %d links %s, want %d",
			links, phase, expectedLinks,
		)
	}
	return nil
}

func validateRollbackIdentityFileInfo(
	info os.FileInfo,
	expectedUID uint64,
	allowUnlinked bool,
) (uint64, error) {
	if !info.Mode().IsRegular() {
		return 0, errors.New("nova: rollback key is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return 0, fmt.Errorf(
			"nova: rollback key mode %04o permits group or world access",
			info.Mode().Perm(),
		)
	}
	if err := requireIdentityOwner(info, expectedUID, "rollback key"); err != nil {
		return 0, err
	}
	links, err := identityStatUint(info, "Nlink")
	if err != nil {
		return 0, fmt.Errorf("nova: rollback key link metadata: %w", err)
	}
	if allowUnlinked && links == 0 {
		return 0, nil
	}
	if links != 1 && links != 2 {
		return links, fmt.Errorf("nova: rollback key has %d links", links)
	}
	return links, nil
}

func validateReservedIdentityNames(keyName string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, errors.New("nova: reserved identity files are missing")
	}
	seen := make(map[string]struct{}, len(names))
	validated := make([]string, 0, len(names))
	for _, name := range names {
		if !fs.ValidPath(name) || name == "." || filepath.Base(name) != name ||
			name == keyName {
			return nil, errors.New("nova: reserved identity file name is invalid")
		}
		if _, ok := seen[name]; ok {
			return nil, errors.New("nova: reserved identity file name is repeated")
		}
		seen[name] = struct{}{}
		validated = append(validated, name)
	}
	return validated, nil
}

func ensureReservedIdentityFilesAbsent(
	dir *identityDirectory,
	names []string,
	policy identityFilePolicy,
) error {
	if err := dir.revalidate(policy); err != nil {
		return err
	}
	entries, err := fs.ReadDir(dir.root.FS(), ".")
	if err != nil {
		return fmt.Errorf("nova: list identity directory: %w", err)
	}
	for _, entry := range entries {
		for _, name := range names {
			if entry.Name() == name ||
				strings.HasPrefix(entry.Name(), "."+name+".tmp-") {
				return fmt.Errorf("%w: %s", ErrIdentityCreationBlocked, entry.Name())
			}
		}
	}
	return dir.revalidate(policy)
}

func openOrCreateIdentityDirectory(
	path string,
	policy identityFilePolicy,
	ops identityFileOps,
) (*identityDirectory, bool, error) {
	parentPath := filepath.Dir(path)
	if parentPath == path {
		dir, err := openIdentityDirectoryPath(path, policy.directoryOwnerUID, false)
		return dir, false, err
	}

	parent, err := openIdentityDirectoryPath(parentPath, policy.directoryOwnerUID, true)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, errors.New("nova: key directory parent must exist")
		}
		return nil, false, fmt.Errorf("nova: open key directory parent: %w", err)
	}
	name := filepath.Base(path)
	created := false
	if _, err := parent.root.Lstat(name); errors.Is(err, os.ErrNotExist) {
		if err := parent.root.Mkdir(name, 0o700); err != nil {
			parent.close()
			return nil, false, fmt.Errorf("nova: mkdir keydir: %w", err)
		}
		created = true
		if err := ops.syncDirectory(parent.file); err != nil {
			parent.close()
			return nil, false, fmt.Errorf("nova: sync new key directory parent: %w", err)
		}
	} else if err != nil {
		parent.close()
		return nil, false, fmt.Errorf("nova: lstat keydir entry: %w", err)
	}

	dir, err := openIdentityDirectoryEntry(parent, name, path, policy.directoryOwnerUID)
	if err != nil {
		parent.close()
		return nil, false, err
	}
	return dir, created, nil
}

func openIdentityDirectoryPath(path string, expectedUID uint64, allowRootOwner bool) (*identityDirectory, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("nova: lstat directory: %w", err)
	}
	if err := validateIdentityDirectoryInfo(info, expectedUID, allowRootOwner); err != nil {
		return nil, err
	}
	opened, err := openIdentityDirectoryFileNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("nova: open directory: %w", err)
	}
	openedInfo, err := opened.Stat()
	if err != nil {
		opened.Close()
		return nil, fmt.Errorf("nova: fstat directory: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		opened.Close()
		return nil, errors.New("nova: directory changed while opening")
	}
	if err := validateIdentityDirectoryInfo(openedInfo, expectedUID, allowRootOwner); err != nil {
		opened.Close()
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		opened.Close()
		return nil, fmt.Errorf("nova: open directory root: %w", err)
	}
	rootInfo, err := root.Lstat(".")
	if err != nil {
		root.Close()
		opened.Close()
		return nil, fmt.Errorf("nova: stat directory root: %w", err)
	}
	if !os.SameFile(openedInfo, rootInfo) {
		root.Close()
		opened.Close()
		return nil, errors.New("nova: directory root changed while opening")
	}
	return &identityDirectory{path: path, file: opened, root: root}, nil
}

func openIdentityDirectoryEntry(
	parent *identityDirectory,
	name string,
	path string,
	expectedUID uint64,
) (*identityDirectory, error) {
	info, err := parent.root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("nova: lstat keydir entry: %w", err)
	}
	if err := validateIdentityDirectoryInfo(info, expectedUID, false); err != nil {
		return nil, err
	}
	opened, err := openIdentityRootDirectoryNoFollow(parent.root, name)
	if err != nil {
		return nil, fmt.Errorf("nova: open keydir entry: %w", err)
	}
	openedInfo, err := opened.Stat()
	if err != nil {
		opened.Close()
		return nil, fmt.Errorf("nova: fstat keydir entry: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		opened.Close()
		return nil, errors.New("nova: key directory entry changed while opening")
	}
	if err := validateIdentityDirectoryInfo(openedInfo, expectedUID, false); err != nil {
		opened.Close()
		return nil, err
	}
	root, err := parent.root.OpenRoot(name)
	if err != nil {
		opened.Close()
		return nil, fmt.Errorf("nova: open key directory root: %w", err)
	}
	rootInfo, err := root.Lstat(".")
	if err != nil {
		root.Close()
		opened.Close()
		return nil, fmt.Errorf("nova: stat key directory root: %w", err)
	}
	if !os.SameFile(openedInfo, rootInfo) {
		root.Close()
		opened.Close()
		return nil, errors.New("nova: key directory root changed while opening")
	}
	return &identityDirectory{
		path: path, file: opened, root: root, parent: parent, entryName: name,
	}, nil
}

func installIdentityFileNoReplace(
	dir *identityDirectory,
	name string,
	data []byte,
	perm fs.FileMode,
	policy identityFilePolicy,
	ops identityFileOps,
) (installed bool, returnErr error) {
	tmp, tmpName, err := createIdentityTempFile(dir, "."+name+".tmp-", perm)
	if err != nil {
		return false, err
	}
	removeOnReturn := true
	defer func() {
		if removeOnReturn {
			removeErr := ops.remove(dir.root, tmpName)
			if errors.Is(removeErr, os.ErrNotExist) {
				removeErr = nil
			}
			returnErr = errors.Join(
				returnErr,
				wrapIdentityError("remove unused key temp file", removeErr),
				wrapIdentityError("sync unused key temp cleanup", ops.syncDirectory(dir.file)),
			)
		}
	}()

	if err := validateIdentityFileDescriptor(tmp, policy.fileOwnerUID, false); err != nil {
		tmp.Close()
		return false, err
	}
	n, err := tmp.Write(data)
	if err != nil {
		tmp.Close()
		return false, err
	}
	if n != len(data) {
		tmp.Close()
		return false, io.ErrShortWrite
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}

	if ops.beforeLink != nil {
		if err := ops.beforeLink(); err != nil {
			return false, err
		}
	}
	if err := ops.link(dir.root, tmpName, name); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return false, fmt.Errorf("install key without replace: %w", err)
		}
		if err := ops.remove(dir.root, tmpName); err != nil {
			return false, fmt.Errorf("remove losing key temp file: %w", err)
		}
		removeOnReturn = false
		if err := waitForDurableIdentity(dir, name, policy, ops); err != nil {
			return false, err
		}
		return false, nil
	}

	// Keep the linked temp name until the first directory barrier has run.
	// This order makes the target link durable before cleanup, then makes the
	// cleanup durable before any caller receives the key.
	removeOnReturn = false
	linkSyncErr := ops.syncDirectory(dir.file)
	cleanupErr := ops.remove(dir.root, tmpName)
	if errors.Is(cleanupErr, os.ErrNotExist) {
		cleanupErr = nil
	}
	cleanupSyncErr := ops.syncDirectory(dir.file)
	if linkSyncErr != nil || cleanupErr != nil || cleanupSyncErr != nil {
		return true, errors.Join(
			wrapIdentityError("sync installed key link", linkSyncErr),
			wrapIdentityError("remove installed key temp file", cleanupErr),
			wrapIdentityError("sync installed key cleanup", cleanupSyncErr),
		)
	}
	return true, nil
}

func createIdentityTempFile(
	dir *identityDirectory,
	prefix string,
	perm fs.FileMode,
) (*os.File, string, error) {
	flag, err := identityNoFollowFlag()
	if err != nil {
		return nil, "", err
	}
	for range 100 {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("nova: random key temp name: %w", err)
		}
		name := prefix + hex.EncodeToString(suffix[:])
		file, err := dir.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|flag, perm)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if err := file.Chmod(perm); err != nil {
			closeErr := file.Close()
			removeErr := dir.root.Remove(name)
			syncErr := dir.file.Sync()
			return nil, "", errors.Join(
				err,
				wrapIdentityError("close unusable key temp file", closeErr),
				wrapIdentityError("remove unusable key temp file", removeErr),
				wrapIdentityError("sync unusable key temp cleanup", syncErr),
			)
		}
		return file, name, nil
	}
	return nil, "", errors.New("nova: could not allocate key temp name")
}

func waitForDurableIdentity(
	dir *identityDirectory,
	name string,
	policy identityFilePolicy,
	ops identityFileOps,
) error {
	if err := ops.syncDirectory(dir.file); err != nil {
		return fmt.Errorf("sync existing key link: %w", err)
	}
	deadline := ops.now().Add(policy.barrierTimeout)
	for {
		info, err := dir.root.Lstat(name)
		if err != nil {
			return fmt.Errorf("lstat existing key: %w", err)
		}
		links, err := validateIdentityFileInfo(info, policy.fileOwnerUID, true)
		if err != nil {
			return err
		}
		if links == 1 {
			if err := ops.syncDirectory(dir.file); err != nil {
				return fmt.Errorf("sync existing key cleanup: %w", err)
			}
			return nil
		}
		if !hasIdentityInstallTemp(dir, name, info) {
			// Cleanup can remove the temp name after Lstat reports two
			// links. Recheck the target before treating it as an unsafe
			// pre-existing hard link.
			confirmed, err := dir.root.Lstat(name)
			if err != nil {
				return fmt.Errorf("lstat existing key after temp check: %w", err)
			}
			confirmedLinks, err := validateIdentityFileInfo(confirmed, policy.fileOwnerUID, true)
			if err != nil {
				return err
			}
			if confirmedLinks == 1 {
				continue
			}
			return fmt.Errorf("nova: key has %d links", confirmedLinks)
		}
		if !ops.now().Before(deadline) {
			return errors.New("nova: timed out waiting for key install cleanup")
		}
		ops.sleep(policy.barrierPollPeriod)
	}
}

func hasIdentityInstallTemp(dir *identityDirectory, name string, targetInfo os.FileInfo) bool {
	prefix := "." + name + ".tmp-"
	entries, err := fs.ReadDir(dir.root.FS(), ".")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := dir.root.Lstat(entry.Name())
		if err == nil && os.SameFile(targetInfo, info) {
			return true
		}
	}
	return false
}

func loadIdentity(dir *identityDirectory, name string, policy identityFilePolicy) (*Identity, error) {
	return loadIdentityWithHook(dir, name, policy, nil)
}

func loadIdentityWithHook(
	dir *identityDirectory,
	name string,
	policy identityFilePolicy,
	afterOpen func() error,
) (*Identity, error) {
	lstatInfo, err := dir.root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("nova: lstat key: %w", err)
	}
	if _, err := validateIdentityFileInfo(lstatInfo, policy.fileOwnerUID, false); err != nil {
		return nil, err
	}

	file, err := openIdentityRootFileNoFollow(dir.root, name)
	if err != nil {
		return nil, fmt.Errorf("nova: open key: %w", err)
	}
	defer file.Close()
	fstatInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("nova: fstat key: %w", err)
	}
	if !os.SameFile(lstatInfo, fstatInfo) {
		return nil, errors.New("nova: key changed while opening")
	}
	if _, err := validateIdentityFileInfo(fstatInfo, policy.fileOwnerUID, false); err != nil {
		return nil, err
	}
	if afterOpen != nil {
		if err := afterOpen(); err != nil {
			return nil, err
		}
	}

	b, err := io.ReadAll(io.LimitReader(file, maxIdentityKeyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("nova: read key: %w", err)
	}
	if len(b) > maxIdentityKeyBytes {
		return nil, errors.New("nova: key file is too large")
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("nova: %s: no PEM block", filepath.Join(dir.path, name))
	}
	priv, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nova: parse key: %w", err)
	}
	if priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("nova: key is not P-256")
	}
	finalInfo, err := dir.root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("nova: revalidate key entry: %w", err)
	}
	if !os.SameFile(fstatInfo, finalInfo) {
		return nil, errors.New("nova: key changed before return")
	}
	if _, err := validateIdentityFileInfo(finalInfo, policy.fileOwnerUID, false); err != nil {
		return nil, err
	}
	return &Identity{priv: priv}, nil
}

func openIdentityFileNoFollow(path string) (*os.File, error) {
	flag, err := identityNoFollowFlag()
	if err != nil {
		return nil, err
	}
	nonblock, err := identityNonblockFlag()
	if err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDONLY|flag|nonblock, 0)
}

func openIdentityRootFileNoFollow(root *os.Root, name string) (*os.File, error) {
	flag, err := identityNoFollowFlag()
	if err != nil {
		return nil, err
	}
	nonblock, err := identityNonblockFlag()
	if err != nil {
		return nil, err
	}
	return root.OpenFile(name, os.O_RDONLY|flag|nonblock, 0)
}

func openIdentityDirectoryFileNoFollow(path string) (*os.File, error) {
	flags, err := identityDirectoryFlags()
	if err != nil {
		return nil, err
	}
	return os.OpenFile(path, flags, 0)
}

func openIdentityRootDirectoryNoFollow(root *os.Root, name string) (*os.File, error) {
	flags, err := identityDirectoryFlags()
	if err != nil {
		return nil, err
	}
	return root.OpenFile(name, flags, 0)
}

// The os package does not expose O_NOFOLLOW. These values come from the
// supported Darwin and Linux kernel ABIs. Other hosts fail closed.
func identityNoFollowFlag() (int, error) {
	switch runtime.GOOS {
	case "darwin":
		return 0x100, nil
	case "linux":
		switch runtime.GOARCH {
		case "arm", "arm64", "ppc64", "ppc64le":
			return 0x8000, nil
		case "386", "amd64", "loong64", "mips", "mips64", "mips64le", "mipsle", "riscv64", "s390x":
			return 0x20000, nil
		}
	}
	return 0, fmt.Errorf("nova: key storage does not support %s/%s", runtime.GOOS, runtime.GOARCH)
}

func identityNonblockFlag() (int, error) {
	switch runtime.GOOS {
	case "darwin":
		return 0x4, nil
	case "linux":
		return 0x800, nil
	default:
		return 0, fmt.Errorf("nova: key storage does not support %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

func identityDirectoryFlags() (int, error) {
	noFollow, err := identityNoFollowFlag()
	if err != nil {
		return 0, err
	}
	nonblock, err := identityNonblockFlag()
	if err != nil {
		return 0, err
	}
	var directory int
	switch runtime.GOOS {
	case "darwin":
		directory = 0x100000
	case "linux":
		switch runtime.GOARCH {
		case "arm", "arm64", "ppc64", "ppc64le":
			directory = 0x4000
		case "386", "amd64", "loong64", "mips", "mips64", "mips64le",
			"mipsle", "riscv64", "s390x":
			directory = 0x10000
		default:
			return 0, fmt.Errorf(
				"nova: key storage does not support %s/%s", runtime.GOOS, runtime.GOARCH,
			)
		}
	default:
		return 0, fmt.Errorf("nova: key storage does not support %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return os.O_RDONLY | noFollow | nonblock | directory, nil
}

func validateIdentityFileDescriptor(file *os.File, expectedUID uint64, allowInstallLink bool) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("nova: fstat key temp file: %w", err)
	}
	_, err = validateIdentityFileInfo(info, expectedUID, allowInstallLink)
	return err
}

func validateIdentityFileInfo(info os.FileInfo, expectedUID uint64, allowInstallLink bool) (uint64, error) {
	if !info.Mode().IsRegular() {
		return 0, errors.New("nova: key is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return 0, fmt.Errorf("nova: key mode %04o permits group or world access", info.Mode().Perm())
	}
	if err := requireIdentityOwner(info, expectedUID, "key"); err != nil {
		return 0, err
	}
	links, err := identityStatUint(info, "Nlink")
	if err != nil {
		return 0, fmt.Errorf("nova: key link metadata: %w", err)
	}
	if links < 1 || (!allowInstallLink && links != 1) {
		return links, fmt.Errorf("nova: key has %d links", links)
	}
	return links, nil
}

func requireIdentityOwner(info os.FileInfo, expectedUID uint64, subject string) error {
	uid, err := identityStatUint(info, "Uid")
	if err != nil {
		return fmt.Errorf("nova: %s owner metadata: %w", subject, err)
	}
	if uid != expectedUID {
		return fmt.Errorf("nova: %s owner is %d, want %d", subject, uid, expectedUID)
	}
	return nil
}

func validateIdentityDirectoryInfo(info os.FileInfo, expectedUID uint64, allowRootOwner bool) error {
	if !info.IsDir() {
		return errors.New("nova: key directory is not a directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("nova: key directory mode %04o permits group or world writes", info.Mode().Perm())
	}
	uid, err := identityStatUint(info, "Uid")
	if err != nil {
		return fmt.Errorf("nova: key directory owner metadata: %w", err)
	}
	if uid != expectedUID && !(allowRootOwner && uid == 0) {
		return fmt.Errorf("nova: key directory owner is %d, want %d", uid, expectedUID)
	}
	return nil
}

func identityStatUint(info os.FileInfo, field string) (uint64, error) {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return 0, errors.New("unavailable")
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, errors.New("unavailable")
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 0, errors.New("unavailable")
	}
	value = value.FieldByName(field)
	if !value.IsValid() {
		return 0, errors.New("unavailable")
	}
	switch value.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if value.Int() < 0 {
			return 0, errors.New("invalid")
		}
		return uint64(value.Int()), nil
	default:
		return 0, errors.New("unavailable")
	}
}

func (dir *identityDirectory) revalidate(policy identityFilePolicy) error {
	fileInfo, err := dir.file.Stat()
	if err != nil {
		return fmt.Errorf("nova: fstat key directory before return: %w", err)
	}
	if err := validateIdentityDirectoryInfo(fileInfo, policy.directoryOwnerUID, false); err != nil {
		return err
	}
	rootInfo, err := dir.root.Lstat(".")
	if err != nil {
		return fmt.Errorf("nova: stat key directory root before return: %w", err)
	}
	if !os.SameFile(fileInfo, rootInfo) {
		return errors.New("nova: key directory handles changed before return")
	}

	if dir.parent == nil {
		pathInfo, err := os.Lstat(dir.path)
		if err != nil {
			return fmt.Errorf("nova: lstat key directory before return: %w", err)
		}
		if !os.SameFile(fileInfo, pathInfo) {
			return errors.New("nova: key directory path changed before return")
		}
		return validateIdentityDirectoryInfo(pathInfo, policy.directoryOwnerUID, false)
	}
	if err := dir.parent.revalidateAsParent(policy); err != nil {
		return err
	}
	entryInfo, err := dir.parent.root.Lstat(dir.entryName)
	if err != nil {
		return fmt.Errorf("nova: lstat key directory entry before return: %w", err)
	}
	if !os.SameFile(fileInfo, entryInfo) {
		return errors.New("nova: key directory entry changed before return")
	}
	return validateIdentityDirectoryInfo(entryInfo, policy.directoryOwnerUID, false)
}

func (dir *identityDirectory) revalidateAsParent(policy identityFilePolicy) error {
	fileInfo, err := dir.file.Stat()
	if err != nil {
		return fmt.Errorf("nova: fstat key directory parent before return: %w", err)
	}
	if err := validateIdentityDirectoryInfo(fileInfo, policy.directoryOwnerUID, true); err != nil {
		return err
	}
	pathInfo, err := os.Lstat(dir.path)
	if err != nil {
		return fmt.Errorf("nova: lstat key directory parent before return: %w", err)
	}
	if !os.SameFile(fileInfo, pathInfo) {
		return errors.New("nova: key directory parent path changed before return")
	}
	return validateIdentityDirectoryInfo(pathInfo, policy.directoryOwnerUID, true)
}

func (dir *identityDirectory) close() {
	if dir == nil {
		return
	}
	if dir.root != nil {
		_ = dir.root.Close()
	}
	if dir.file != nil {
		_ = dir.file.Close()
	}
	if dir.parent != nil {
		dir.parent.close()
	}
}

func wrapIdentityError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

func publicKeysEqual(a, b *ecdsa.PrivateKey) bool {
	return a.PublicKey.X.Cmp(b.PublicKey.X) == 0 && a.PublicKey.Y.Cmp(b.PublicKey.Y) == 0
}

// PublicKeyHex returns the uncompressed P-256 public key as the 64-byte
// X||Y hex string (128 hex chars) that Nova's auth methods table stores.
// This is what POST /gateways/claim expects in the `public_key` field.
func (id *Identity) PublicKeyHex() string {
	x := padBig(id.priv.X, 32)
	y := padBig(id.priv.Y, 32)
	buf := make([]byte, 0, 64)
	buf = append(buf, x...)
	buf = append(buf, y...)
	return hex.EncodeToString(buf)
}

// SignRawHex signs msg with the identity's ES256 key and returns the raw
// R||S 64-byte signature as a 128-char hex string. This is the exact
// format Nova's ownership.verifyES256Signature expects for claim proofs.
//
// DOMAIN SEPARATION: this key also signs JWTs. Every raw-message caller must
// prefix msg with a unique, versioned purpose tag. Never pass
// attacker-influenced bytes without that prefix.
func (id *Identity) SignRawHex(msg string) (string, error) {
	hash := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, id.priv, hash[:])
	if err != nil {
		return "", fmt.Errorf("nova: sign: %w", err)
	}
	sig := make([]byte, 64)
	rb := r.Bytes()
	sb := s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	return hex.EncodeToString(sig), nil
}

func padBig(x *big.Int, size int) []byte {
	b := x.Bytes()
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}
