//go:build linux

package gatewayidentity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/srcfl/ftw/go/internal/nova"
)

type linuxBindingStorage struct {
	parentPath string
	parentDir  *os.File
	parentRoot *os.Root
	entryName  string
	path       string
	dir        *os.File
	root       *os.Root
	ownerUID   uint32
	ops        bindingFileOps
}

func defaultBindingFileOps() bindingFileOps {
	return bindingFileOps{
		syncFile: func(file fileSyncer) error { return file.Sync() },
		syncDirectory: func(file fileSyncer) error {
			return file.Sync()
		},
		randomSuffix: func() (string, error) {
			var raw [16]byte
			if _, err := rand.Read(raw[:]); err != nil {
				return "", err
			}
			return hex.EncodeToString(raw[:]), nil
		},
	}
}

// LoadOrCreateUnboundNovaIdentity keeps the binding absence check and the
// optional nova.key install under the same pinned directory lock.
func LoadOrCreateUnboundNovaIdentity(keyPath string) (*nova.Identity, error) {
	paths, err := PathsForKey(keyPath)
	if err != nil {
		return nil, err
	}
	identity, err := nova.LoadOrCreateIdentityGuarded(
		paths.Key,
		[]string{filepath.Base(paths.Marker), filepath.Base(paths.Sidecar)},
		lockLinuxBindingDirectory,
	)
	if errors.Is(err, nova.ErrIdentityCreationBlocked) {
		return nil, ErrBindingIncomplete
	}
	return identity, err
}

func openBindingStorage(paths BindingPaths, ops bindingFileOps) (bindingStorage, error) {
	dirPath := filepath.Dir(paths.Key)
	for _, path := range []string{paths.Sidecar, paths.Marker} {
		if filepath.Dir(path) != dirPath {
			return nil, errors.New("binding files must share the canonical key directory")
		}
	}
	parentPath := filepath.Dir(dirPath)
	entryName := filepath.Base(dirPath)
	if parentPath == dirPath || !fs.ValidPath(entryName) || entryName == "." {
		return nil, errors.New("binding directory must have a trusted parent")
	}
	uid := uint32(os.Geteuid())
	parentDir, parentRoot, err := openPinnedLinuxDirectory(parentPath, uid)
	if err != nil {
		return nil, fmt.Errorf("open binding parent directory: %w", err)
	}
	closeParent := true
	defer func() {
		if closeParent {
			_ = parentRoot.Close()
			_ = parentDir.Close()
		}
	}()
	entryInfo, err := parentRoot.Lstat(entryName)
	if err != nil {
		return nil, fmt.Errorf("lstat binding directory entry: %w", err)
	}
	if err := validateLinuxBindingDirectory(entryInfo, uid); err != nil {
		return nil, err
	}
	dirFlags, err := linuxBindingDirectoryFlags()
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Openat(int(parentDir.Fd()), entryName, dirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("open binding directory relative to parent: %w", err)
	}
	dir := os.NewFile(uintptr(fd), dirPath)
	if dir == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open binding directory returned no file")
	}
	openedInfo, err := dir.Stat()
	if err != nil {
		dir.Close()
		return nil, fmt.Errorf("fstat binding directory: %w", err)
	}
	if !os.SameFile(entryInfo, openedInfo) {
		dir.Close()
		return nil, errors.New("binding directory changed while opening")
	}
	if err := validateLinuxBindingDirectory(openedInfo, uid); err != nil {
		dir.Close()
		return nil, err
	}
	root, err := parentRoot.OpenRoot(entryName)
	if err != nil {
		dir.Close()
		return nil, fmt.Errorf("open binding directory root relative to parent: %w", err)
	}
	rootInfo, err := root.Lstat(".")
	if err != nil {
		root.Close()
		dir.Close()
		return nil, fmt.Errorf("fstat binding directory root: %w", err)
	}
	if !os.SameFile(openedInfo, rootInfo) {
		root.Close()
		dir.Close()
		return nil, errors.New("binding directory root changed while opening")
	}
	store := &linuxBindingStorage{
		parentPath: parentPath, parentDir: parentDir, parentRoot: parentRoot,
		entryName: entryName, path: dirPath, dir: dir, root: root,
		ownerUID: uid, ops: ops,
	}
	if store.ops.syncFile == nil {
		store.ops.syncFile = func(file fileSyncer) error { return file.Sync() }
	}
	if store.ops.syncDirectory == nil {
		store.ops.syncDirectory = func(file fileSyncer) error { return file.Sync() }
	}
	if store.ops.randomSuffix == nil {
		store.ops.randomSuffix = defaultBindingFileOps().randomSuffix
	}
	closeParent = false
	if err := store.Revalidate(); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *linuxBindingStorage) Lock() error {
	if err := lockLinuxBindingDirectory(s.dir); err != nil {
		return fmt.Errorf("lock binding directory: %w", err)
	}
	return s.Revalidate()
}

func lockLinuxBindingDirectory(dir *os.File) error {
	for {
		err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return err
	}
}

func (s *linuxBindingStorage) Close() error {
	var errs []error
	if s.root != nil {
		errs = append(errs, s.root.Close())
	}
	if s.dir != nil {
		errs = append(errs, s.dir.Close())
	}
	if s.parentRoot != nil {
		errs = append(errs, s.parentRoot.Close())
	}
	if s.parentDir != nil {
		errs = append(errs, s.parentDir.Close())
	}
	return errors.Join(errs...)
}

func (s *linuxBindingStorage) Revalidate() error {
	parentPathInfo, err := os.Lstat(s.parentPath)
	if err != nil {
		return fmt.Errorf("revalidate binding parent entry: %w", err)
	}
	parentFDInfo, err := s.parentDir.Stat()
	if err != nil {
		return fmt.Errorf("revalidate binding parent descriptor: %w", err)
	}
	parentRootInfo, err := s.parentRoot.Lstat(".")
	if err != nil {
		return fmt.Errorf("revalidate binding parent root: %w", err)
	}
	if !os.SameFile(parentPathInfo, parentFDInfo) ||
		!os.SameFile(parentFDInfo, parentRootInfo) {
		return errors.New("binding parent entry no longer matches its descriptor")
	}
	if err := validateLinuxBindingParentDirectory(parentFDInfo, s.ownerUID); err != nil {
		return err
	}
	entryInfo, err := s.parentRoot.Lstat(s.entryName)
	if err != nil {
		return fmt.Errorf("revalidate binding directory parent entry: %w", err)
	}
	fdInfo, err := s.dir.Stat()
	if err != nil {
		return fmt.Errorf("revalidate binding directory descriptor: %w", err)
	}
	rootInfo, err := s.root.Lstat(".")
	if err != nil {
		return fmt.Errorf("revalidate binding directory root: %w", err)
	}
	if !os.SameFile(entryInfo, fdInfo) || !os.SameFile(fdInfo, rootInfo) {
		return errors.New("binding directory entry no longer matches its descriptor")
	}
	if err := validateLinuxBindingDirectory(entryInfo, s.ownerUID); err != nil {
		return err
	}
	return validateLinuxBindingDirectory(fdInfo, s.ownerUID)
}

func openPinnedLinuxDirectory(
	path string,
	uid uint32,
) (*os.File, *os.Root, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if err := validateLinuxBindingParentDirectory(pathInfo, uid); err != nil {
		return nil, nil, err
	}
	flags, err := linuxBindingDirectoryFlags()
	if err != nil {
		return nil, nil, err
	}
	fd, err := syscall.Open(path, flags, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, nil, errors.New("open directory returned no file")
	}
	fileInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !os.SameFile(pathInfo, fileInfo) {
		file.Close()
		return nil, nil, errors.New("directory changed while opening")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	rootInfo, err := root.Lstat(".")
	if err != nil || !os.SameFile(fileInfo, rootInfo) {
		root.Close()
		file.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("directory root differs from descriptor")
	}
	return file, root, nil
}

func (s *linuxBindingStorage) Read(name string) ([]byte, error) {
	if err := validBindingName(name); err != nil {
		return nil, err
	}
	if err := s.Revalidate(); err != nil {
		return nil, err
	}
	return s.read(name, true)
}

func (s *linuxBindingStorage) read(name string, allowCleanup bool) ([]byte, error) {
	lstatInfo, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	links, err := validateLinuxBindingFile(lstatInfo, s.ownerUID, true)
	if err != nil {
		return nil, err
	}
	if links == 2 {
		if !allowCleanup {
			return nil, fmt.Errorf("binding file %s still has an install link", name)
		}
		if err := s.finishInstallCleanup(name, lstatInfo); err != nil {
			return nil, err
		}
		return s.read(name, false)
	}
	if links != 1 {
		return nil, fmt.Errorf("binding file %s has %d links", name, links)
	}
	noFollow, err := linuxBindingNoFollow()
	if err != nil {
		return nil, err
	}
	file, err := s.root.OpenFile(name, os.O_RDONLY|noFollow|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fstatInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("fstat binding file %s: %w", name, err)
	}
	if !os.SameFile(lstatInfo, fstatInfo) {
		return nil, fmt.Errorf("binding file %s changed while opening", name)
	}
	if _, err := validateLinuxBindingFile(fstatInfo, s.ownerUID, false); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBindingFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read binding file %s: %w", name, err)
	}
	if len(data) > maxBindingFileBytes {
		return nil, fmt.Errorf("binding file %s is too large", name)
	}
	// This barrier lets an EEXIST loser wait for the winning directory entry
	// before it loads or returns.
	if err := s.ops.syncDirectory(s.dir); err != nil {
		return nil, fmt.Errorf("sync binding directory before reading %s: %w", name, err)
	}
	finalInfo, err := s.root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("revalidate binding file %s entry: %w", name, err)
	}
	if !os.SameFile(fstatInfo, finalInfo) {
		return nil, fmt.Errorf("binding file %s changed before return", name)
	}
	if _, err := validateLinuxBindingFile(finalInfo, s.ownerUID, false); err != nil {
		return nil, err
	}
	if err := s.Revalidate(); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *linuxBindingStorage) InstallNoReplace(name string, data []byte) error {
	if err := validBindingName(name); err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxBindingFileBytes {
		return fmt.Errorf("binding file %s has invalid size", name)
	}
	if err := s.Revalidate(); err != nil {
		return err
	}
	if _, err := s.root.Lstat(name); err == nil {
		if _, readErr := s.Read(name); readErr != nil {
			return readErr
		}
		if cleanupErr := s.removeUnlinkedTemps(name); cleanupErr != nil {
			return cleanupErr
		}
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	suffix, err := s.ops.randomSuffix()
	if err != nil {
		return fmt.Errorf("create binding temp name: %w", err)
	}
	tempName := "." + name + ".tmp-" + suffix
	noFollow, err := linuxBindingNoFollow()
	if err != nil {
		return err
	}
	temp, err := s.root.OpenFile(
		tempName,
		os.O_RDWR|os.O_CREATE|os.O_EXCL|noFollow|syscall.O_NONBLOCK,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("create binding temp file: %w", err)
	}
	tempOpen := true
	linked := false
	if err := syscall.Flock(int(temp.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = temp.Close()
		return fmt.Errorf("lock binding temp file: %w", err)
	}
	defer func() {
		if tempOpen {
			_ = temp.Close()
		}
		if !linked {
			if s.remove(tempName) == nil {
				_ = s.ops.syncDirectory(s.dir)
			}
		}
	}()
	written, err := temp.Write(data)
	if err != nil {
		return fmt.Errorf("write binding temp file: %w", err)
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	if err := s.ops.syncFile(temp); err != nil {
		return fmt.Errorf("sync binding temp file: %w", err)
	}
	tempInfo, err := temp.Stat()
	if err != nil {
		return fmt.Errorf("fstat binding temp file: %w", err)
	}
	if links, err := validateLinuxBindingFile(tempInfo, s.ownerUID, false); err != nil || links != 1 {
		if links == 0 && s.targetExists(name) {
			return fs.ErrExist
		}
		if err != nil {
			return err
		}
		return errors.New("binding temp file has unexpected links")
	}
	if err := s.link(tempName, name); err != nil {
		if errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrNotExist) {
			if removeErr := s.remove(tempName); removeErr != nil &&
				!errors.Is(removeErr, fs.ErrNotExist) {
				return fmt.Errorf("remove losing binding temp file: %w", removeErr)
			}
			if syncErr := s.ops.syncDirectory(s.dir); syncErr != nil {
				return fmt.Errorf("sync losing binding install: %w", syncErr)
			}
			if _, readErr := s.Read(name); readErr != nil {
				return fmt.Errorf("load winning binding file: %w", readErr)
			}
			return fs.ErrExist
		}
		return fmt.Errorf("link binding file without replace: %w", err)
	}
	linked = true
	// The target entry must be durable before cleanup removes the only other
	// link to the synced inode.
	if err := s.ops.syncDirectory(s.dir); err != nil {
		return fmt.Errorf("sync linked binding file: %w", err)
	}
	target, err := s.readLinkedTarget(name, tempInfo)
	if err != nil {
		return err
	}
	if !bytes.Equal(target, data) {
		return errors.New("linked binding file does not match temp data")
	}
	if err := s.remove(tempName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove linked binding temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close binding temp file: %w", err)
	}
	tempOpen = false
	if err := s.ops.syncDirectory(s.dir); err != nil {
		return fmt.Errorf("sync binding install cleanup: %w", err)
	}
	if err := s.removeUnlinkedTemps(name); err != nil {
		return err
	}
	persisted, err := s.read(name, false)
	if err != nil {
		return err
	}
	if !bytes.Equal(persisted, data) {
		return errors.New("persisted binding file does not match installed data")
	}
	return s.Revalidate()
}

func (s *linuxBindingStorage) ReplaceExact(
	name string,
	oldData, newData []byte,
) (returnErr error) {
	if err := validBindingName(name); err != nil {
		return err
	}
	if len(oldData) == 0 || len(oldData) > maxBindingFileBytes ||
		len(newData) == 0 || len(newData) > maxBindingFileBytes {
		return fmt.Errorf("binding file %s has invalid transition size", name)
	}
	if err := s.Revalidate(); err != nil {
		return err
	}
	current, err := s.readForTransition(name)
	if err != nil {
		return err
	}
	if bytes.Equal(current, newData) {
		return s.removeUnlinkedTemps(name)
	}
	if !bytes.Equal(current, oldData) {
		return fmt.Errorf("%w: binding marker changed before transition", ErrBindingMismatch)
	}
	suffix, err := s.ops.randomSuffix()
	if err != nil {
		return fmt.Errorf("create binding transition temp name: %w", err)
	}
	tempName := "." + name + ".tmp-" + suffix
	noFollow, err := linuxBindingNoFollow()
	if err != nil {
		return err
	}
	temp, err := s.root.OpenFile(
		tempName,
		os.O_RDWR|os.O_CREATE|os.O_EXCL|noFollow|syscall.O_NONBLOCK,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("create binding transition temp: %w", err)
	}
	tempOpen := true
	renamed := false
	if err := syscall.Flock(int(temp.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = temp.Close()
		return fmt.Errorf("lock binding transition temp: %w", err)
	}
	defer func() {
		var cleanupErrs []error
		if tempOpen {
			cleanupErrs = append(cleanupErrs, temp.Close())
		}
		if !renamed {
			removeErr := s.remove(tempName)
			if removeErr == nil {
				cleanupErrs = append(cleanupErrs, s.ops.syncDirectory(s.dir))
			} else if !errors.Is(removeErr, fs.ErrNotExist) {
				cleanupErrs = append(cleanupErrs, removeErr)
			}
		}
		returnErr = errors.Join(returnErr, errors.Join(cleanupErrs...))
	}()
	written, err := temp.Write(newData)
	if err != nil {
		return fmt.Errorf("write binding transition temp: %w", err)
	}
	if written != len(newData) {
		return io.ErrShortWrite
	}
	if err := s.ops.syncFile(temp); err != nil {
		return fmt.Errorf("sync binding transition temp: %w", err)
	}
	tempInfo, err := temp.Stat()
	if err != nil {
		return fmt.Errorf("fstat binding transition temp: %w", err)
	}
	if links, err := validateLinuxBindingFile(tempInfo, s.ownerUID, false); err != nil || links != 1 {
		if err != nil {
			return err
		}
		return errors.New("binding transition temp has unexpected links")
	}
	// Recheck the immutable pending bytes immediately before the atomic
	// directory-entry transition. Concurrent writers may only install the
	// same active bytes.
	current, err = s.readForTransition(name)
	if err != nil {
		return err
	}
	if bytes.Equal(current, newData) {
		return nil
	}
	if !bytes.Equal(current, oldData) {
		return fmt.Errorf("%w: binding marker changed during transition", ErrBindingMismatch)
	}
	if err := s.rename(tempName, name); err != nil {
		return fmt.Errorf("replace binding marker: %w", err)
	}
	renamed = true
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close binding transition temp: %w", err)
	}
	tempOpen = false
	if err := s.ops.syncDirectory(s.dir); err != nil {
		return fmt.Errorf("sync binding marker transition: %w", err)
	}
	persisted, err := s.Read(name)
	if err != nil {
		return err
	}
	if !bytes.Equal(persisted, newData) {
		return errors.New("persisted binding marker does not match active state")
	}
	if err := s.removeUnlinkedTemps(name); err != nil {
		return err
	}
	return s.Revalidate()
}

func (s *linuxBindingStorage) readForTransition(name string) ([]byte, error) {
	var lastErr error
	for range 16 {
		data, err := s.Read(name)
		if err == nil {
			return data, nil
		}
		lastErr = err
		runtime.Gosched()
	}
	return nil, lastErr
}

func (s *linuxBindingStorage) readLinkedTarget(name string, tempInfo os.FileInfo) ([]byte, error) {
	info, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	links, err := validateLinuxBindingFile(info, s.ownerUID, true)
	if err != nil {
		return nil, err
	}
	if (links != 1 && links != 2) || !os.SameFile(info, tempInfo) {
		return nil, errors.New("linked binding target does not match temp inode")
	}
	noFollow, err := linuxBindingNoFollow()
	if err != nil {
		return nil, err
	}
	file, err := s.root.OpenFile(name, os.O_RDONLY|noFollow|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, openedInfo) {
		return nil, errors.New("linked binding target changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBindingFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBindingFileBytes {
		return nil, errors.New("linked binding target is too large")
	}
	return data, nil
}

func (s *linuxBindingStorage) finishInstallCleanup(name string, targetInfo os.FileInfo) error {
	prefix := "." + name + ".tmp-"
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return fmt.Errorf("list binding install temps: %w", err)
	}
	var matching string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := s.root.Lstat(entry.Name())
		if err != nil {
			continue
		}
		if os.SameFile(info, targetInfo) {
			if matching != "" {
				return errors.New("binding target has more than one install temp")
			}
			matching = entry.Name()
		}
	}
	if matching == "" {
		current, err := s.root.Lstat(name)
		if err == nil {
			if links, validateErr := validateLinuxBindingFile(current, s.ownerUID, false); validateErr == nil &&
				links == 1 {
				return nil
			}
		}
		return errors.New("binding target has an unexplained hard link")
	}
	if err := s.ops.syncDirectory(s.dir); err != nil {
		return fmt.Errorf("sync binding winner before cleanup: %w", err)
	}
	if err := s.remove(matching); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove binding winner temp: %w", err)
	}
	if err := s.ops.syncDirectory(s.dir); err != nil {
		return fmt.Errorf("sync binding winner cleanup: %w", err)
	}
	return nil
}

func (s *linuxBindingStorage) removeUnlinkedTemps(name string) error {
	prefix := "." + name + ".tmp-"
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := s.root.Lstat(entry.Name())
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		links, err := validateLinuxBindingFile(info, s.ownerUID, false)
		if err != nil || links != 1 {
			if err != nil {
				return err
			}
			return errors.New("unlinked binding temp has unexpected links")
		}
		noFollow, err := linuxBindingNoFollow()
		if err != nil {
			return err
		}
		file, err := s.root.OpenFile(
			entry.Name(), os.O_RDONLY|noFollow|syscall.O_NONBLOCK, 0,
		)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		lockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(lockErr, syscall.EWOULDBLOCK) || errors.Is(lockErr, syscall.EAGAIN) {
			_ = file.Close()
			continue
		}
		if lockErr != nil {
			_ = file.Close()
			return lockErr
		}
		lockedInfo, err := file.Stat()
		if err != nil || !os.SameFile(info, lockedInfo) {
			_ = file.Close()
			if err != nil {
				return err
			}
			return errors.New("binding temp changed while locking")
		}
		if err := s.remove(entry.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = file.Close()
			return err
		}
		_ = file.Close()
		removed = true
	}
	if removed {
		if err := s.ops.syncDirectory(s.dir); err != nil {
			return fmt.Errorf("sync unlinked binding temp cleanup: %w", err)
		}
	}
	return nil
}

func (s *linuxBindingStorage) targetExists(name string) bool {
	_, err := s.root.Lstat(name)
	return err == nil
}

func (s *linuxBindingStorage) link(oldName, newName string) error {
	if s.ops.link != nil {
		return s.ops.link(oldName, newName)
	}
	return s.root.Link(oldName, newName)
}

func (s *linuxBindingStorage) rename(oldName, newName string) error {
	if s.ops.rename != nil {
		return s.ops.rename(oldName, newName)
	}
	return s.root.Rename(oldName, newName)
}

func (s *linuxBindingStorage) remove(name string) error {
	if s.ops.remove != nil {
		return s.ops.remove(name)
	}
	return s.root.Remove(name)
}

func validBindingName(name string) error {
	if !fs.ValidPath(name) || name == "." || filepath.Base(name) != name {
		return errors.New("binding file name is invalid")
	}
	return nil
}

func validateLinuxBindingDirectory(info os.FileInfo, expectedUID uint32) error {
	if !info.IsDir() {
		return errors.New("binding directory is not a directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("binding directory mode %04o permits group or world writes", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("binding directory owner metadata is unavailable")
	}
	if stat.Uid != expectedUID {
		return fmt.Errorf("binding directory owner is %d, want %d", stat.Uid, expectedUID)
	}
	return nil
}

func validateLinuxBindingParentDirectory(info os.FileInfo, expectedUID uint32) error {
	if !info.IsDir() {
		return errors.New("binding parent is not a directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf(
			"binding parent mode %04o permits group or world writes", info.Mode().Perm(),
		)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("binding parent owner metadata is unavailable")
	}
	if stat.Uid != expectedUID && stat.Uid != 0 {
		return fmt.Errorf("binding parent owner is %d, want %d or root", stat.Uid, expectedUID)
	}
	return nil
}

func validateLinuxBindingFile(info os.FileInfo, expectedUID uint32, allowInstallLink bool) (uint64, error) {
	if !info.Mode().IsRegular() {
		return 0, errors.New("binding state is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return 0, fmt.Errorf("binding file mode %04o permits group or world access", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("binding file owner metadata is unavailable")
	}
	if stat.Uid != expectedUID {
		return 0, fmt.Errorf("binding file owner is %d, want %d", stat.Uid, expectedUID)
	}
	links := uint64(stat.Nlink)
	if links < 1 || (!allowInstallLink && links != 1) || (allowInstallLink && links > 2) {
		return links, fmt.Errorf("binding file has %d links", links)
	}
	return links, nil
}

func linuxBindingNoFollow() (int, error) {
	switch runtime.GOARCH {
	case "arm", "arm64", "ppc64", "ppc64le":
		return 0x8000, nil
	case "386", "amd64", "loong64", "mips", "mips64", "mips64le", "mipsle", "riscv64", "s390x":
		return 0x20000, nil
	default:
		return 0, fmt.Errorf("home link binding does not support linux/%s", runtime.GOARCH)
	}
}

func linuxBindingDirectoryFlags() (int, error) {
	noFollow, err := linuxBindingNoFollow()
	if err != nil {
		return 0, err
	}
	return syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_DIRECTORY |
		syscall.O_NONBLOCK | noFollow, nil
}

func makeBindingFIFO(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}

type linuxRouteAuthority struct{}

func newRouteAuthority() routeAuthority { return linuxRouteAuthority{} }

func (linuxRouteAuthority) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		ips = append(ips, address.IP)
	}
	return ips, nil
}

func (linuxRouteAuthority) ResolvedRoute(ctx context.Context, ip net.IP) (routeResult, error) {
	return linuxResolvedRoute(ctx, ip)
}

func (linuxRouteAuthority) PhysicalInterface(index int) (physicalInterface, error) {
	iface, err := net.InterfaceByIndex(index)
	if err != nil {
		return physicalInterface{}, err
	}
	if iface.Flags&net.FlagLoopback != 0 {
		return physicalInterface{}, errors.New("route interface is loopback")
	}
	if iface.Flags&net.FlagUp == 0 {
		return physicalInterface{}, errors.New("route interface is down")
	}
	if strings.ContainsAny(iface.Name, `/\`) || iface.Name == "." || iface.Name == ".." {
		return physicalInterface{}, errors.New("route interface name is invalid")
	}
	base := filepath.Join("/sys/class/net", iface.Name)
	linkType, err := readTrimmedSysfs(filepath.Join(base, "type"))
	if err != nil || linkType != "1" {
		return physicalInterface{}, errors.New("route interface is not Ethernet-class physical hardware")
	}
	assignType, err := readTrimmedSysfs(filepath.Join(base, "addr_assign_type"))
	if err != nil || assignType != "0" {
		return physicalInterface{}, errors.New("route interface MAC is assigned or random")
	}
	if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
		return physicalInterface{}, errors.New("route interface has no physical device")
	}
	if _, err := os.Lstat(filepath.Join(base, "master")); err == nil {
		return physicalInterface{}, errors.New("route interface belongs to a bridge, bond, or VRF")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return physicalInterface{}, fmt.Errorf("inspect route interface master: %w", err)
	}
	address, err := readTrimmedSysfs(filepath.Join(base, "address"))
	if err != nil {
		return physicalInterface{}, err
	}
	sysfsCurrent, err := net.ParseMAC(address)
	if err != nil || len(sysfsCurrent) != 6 {
		return physicalInterface{}, errors.New("route interface current sysfs MAC is invalid")
	}
	netlinkCurrent, permanent, netlinkUp, err := linuxPermanentInterfaceAddresses(index)
	if err != nil {
		return physicalInterface{}, fmt.Errorf("read permanent route interface MAC: %w", err)
	}
	if !netlinkUp {
		return physicalInterface{}, errors.New("netlink reports route interface down")
	}
	var sysfsPermanent net.HardwareAddr
	if raw, readErr := readTrimmedSysfs(filepath.Join(base, "perm_address")); readErr == nil {
		sysfsPermanent, err = net.ParseMAC(raw)
		if err != nil || len(sysfsPermanent) != 6 {
			return physicalInterface{}, errors.New("route interface permanent sysfs MAC is invalid")
		}
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return physicalInterface{}, fmt.Errorf("read permanent sysfs MAC: %w", readErr)
	}
	if err := validatePermanentMACSources(
		iface.Flags&net.FlagUp != 0, netlinkUp,
		iface.HardwareAddr, sysfsCurrent, netlinkCurrent, permanent, sysfsPermanent,
	); err != nil {
		return physicalInterface{}, err
	}
	result := physicalInterface{Name: iface.Name, Index: iface.Index, PermanentMAC: permanent}
	if err := validatePhysicalInterface(result); err != nil {
		return physicalInterface{}, err
	}
	return result, nil
}

func linuxPermanentInterfaceAddresses(
	index int,
) (net.HardwareAddr, net.HardwareAddr, bool, error) {
	const iflaPermAddress = 54
	if index <= 0 {
		return nil, nil, false, errors.New("interface index is invalid")
	}
	fd, err := syscall.Socket(
		syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE,
	)
	if err != nil {
		return nil, nil, false, err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, nil, false, err
	}
	var seqRaw [4]byte
	if _, err := rand.Read(seqRaw[:]); err != nil {
		return nil, nil, false, err
	}
	seq := binary.NativeEndian.Uint32(seqRaw[:])
	request := make([]byte, 16+syscall.SizeofIfInfomsg)
	request[16] = syscall.AF_UNSPEC
	binary.NativeEndian.PutUint32(request[20:24], uint32(index))
	binary.NativeEndian.PutUint32(request[0:4], uint32(len(request)))
	binary.NativeEndian.PutUint16(request[4:6], syscall.RTM_GETLINK)
	binary.NativeEndian.PutUint16(request[6:8], syscall.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(request[8:12], seq)
	if err := syscall.Sendto(
		fd, request, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK},
	); err != nil {
		return nil, nil, false, err
	}
	timeout := syscall.NsecToTimeval((100 * time.Millisecond).Nanoseconds())
	if err := syscall.SetsockoptTimeval(
		fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout,
	); err != nil {
		return nil, nil, false, err
	}
	var buffer [64 << 10]byte
	for {
		n, sender, err := syscall.Recvfrom(fd, buffer[:], 0)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return nil, nil, false, err
		}
		kernel, ok := sender.(*syscall.SockaddrNetlink)
		if !ok || kernel.Pid != 0 {
			return nil, nil, false, errors.New("link response did not come from the kernel")
		}
		messages, err := syscall.ParseNetlinkMessage(buffer[:n])
		if err != nil {
			return nil, nil, false, err
		}
		for _, message := range messages {
			if message.Header.Seq != seq {
				continue
			}
			switch message.Header.Type {
			case syscall.NLMSG_ERROR:
				if len(message.Data) < 4 {
					return nil, nil, false, errors.New("short netlink link error")
				}
				code := int32(binary.NativeEndian.Uint32(message.Data[:4]))
				if code != 0 {
					return nil, nil, false, syscall.Errno(-code)
				}
			case syscall.RTM_NEWLINK:
				if len(message.Data) < syscall.SizeofIfInfomsg {
					return nil, nil, false, errors.New("short netlink link response")
				}
				gotIndex := int(int32(binary.NativeEndian.Uint32(message.Data[4:8])))
				if gotIndex != index {
					return nil, nil, false, errors.New("netlink returned a different interface")
				}
				flags := binary.NativeEndian.Uint32(message.Data[8:12])
				attrs, err := syscall.ParseNetlinkRouteAttr(&message)
				if err != nil {
					return nil, nil, false, err
				}
				var current, permanent net.HardwareAddr
				for _, attr := range attrs {
					switch attr.Attr.Type & 0x3fff {
					case syscall.IFLA_ADDRESS:
						current = append(net.HardwareAddr(nil), attr.Value...)
					case iflaPermAddress:
						permanent = append(net.HardwareAddr(nil), attr.Value...)
					}
				}
				if len(permanent) == 0 {
					return nil, nil, false, errors.New("kernel did not provide a permanent MAC")
				}
				return current, permanent, flags&syscall.IFF_UP != 0, nil
			}
		}
	}
}

func readTrimmedSysfs(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 256))
	if err != nil {
		return "", err
	}
	if len(data) >= 256 {
		return "", errors.New("sysfs value is too large")
	}
	return strings.TrimSpace(string(data)), nil
}

func linuxResolvedRoute(ctx context.Context, ip net.IP) (routeResult, error) {
	family := uint8(syscall.AF_INET6)
	dst := ip.To16()
	prefix := uint8(128)
	if v4 := ip.To4(); v4 != nil {
		family = syscall.AF_INET
		dst = v4
		prefix = 32
	}
	if dst == nil {
		return routeResult{}, errors.New("route destination is not an IP address")
	}
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return routeResult{}, err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return routeResult{}, err
	}
	seqBytes := make([]byte, 4)
	if _, err := rand.Read(seqBytes); err != nil {
		return routeResult{}, err
	}
	seq := binary.NativeEndian.Uint32(seqBytes)
	request := marshalRouteRequest(family, prefix, dst, seq, uint32(os.Geteuid()))
	if err := syscall.Sendto(fd, request, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return routeResult{}, err
	}
	timeout := syscall.NsecToTimeval((100 * time.Millisecond).Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout); err != nil {
		return routeResult{}, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return routeResult{}, err
		}
		var buffer [64 << 10]byte
		n, sender, recvErr := syscall.Recvfrom(fd, buffer[:], 0)
		if recvErr != nil {
			if errors.Is(recvErr, syscall.EAGAIN) || errors.Is(recvErr, syscall.EWOULDBLOCK) ||
				errors.Is(recvErr, syscall.EINTR) {
				continue
			}
			return routeResult{}, recvErr
		}
		kernel, ok := sender.(*syscall.SockaddrNetlink)
		if !ok || kernel.Pid != 0 {
			return routeResult{}, errors.New("resolved route response did not come from the kernel")
		}
		messages, parseErr := syscall.ParseNetlinkMessage(buffer[:n])
		if parseErr != nil {
			return routeResult{}, parseErr
		}
		var result routeResult
		found := false
		for _, message := range messages {
			if message.Header.Seq != seq {
				continue
			}
			switch message.Header.Type {
			case syscall.NLMSG_ERROR:
				if len(message.Data) < 4 {
					return routeResult{}, errors.New("short netlink route error")
				}
				code := int32(binary.NativeEndian.Uint32(message.Data[:4]))
				if code != 0 {
					errno := syscall.Errno(-code)
					if errno == syscall.ENETUNREACH || errno == syscall.EHOSTUNREACH ||
						errno == syscall.ESRCH {
						return routeResult{}, fmt.Errorf("%w: %v", ErrNoUsableRoute, errno)
					}
					return routeResult{}, errno
				}
			case syscall.RTM_NEWROUTE:
				parsed, routeErr := parseResolvedRoute(message)
				if routeErr != nil {
					return routeResult{}, routeErr
				}
				if found {
					return routeResult{}, ErrAmbiguousRoute
				}
				result = parsed
				found = true
			}
		}
		if found {
			return result, nil
		}
	}
}

func marshalRouteRequest(family, prefix uint8, dst []byte, seq, uid uint32) []byte {
	const (
		headerLen   = 16
		rtmsgLen    = 12
		rtaUID      = 25
		rtaIPProto  = 27
		rtaDPort    = 29
		rtmFibMatch = 0x2000
	)
	data := make([]byte, headerLen+rtmsgLen)
	data[16] = family
	data[17] = prefix
	data[20] = syscall.RT_TABLE_UNSPEC
	data[23] = syscall.RTN_UNICAST
	binary.NativeEndian.PutUint32(data[24:28], rtmFibMatch)
	data = appendRouteAttribute(data, syscall.RTA_DST, dst)
	var uidBytes [4]byte
	binary.NativeEndian.PutUint32(uidBytes[:], uid)
	data = appendRouteAttribute(data, rtaUID, uidBytes[:])
	data = appendRouteAttribute(data, rtaIPProto, []byte{syscall.IPPROTO_TCP})
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], SoftwareIdentityUplinkPort)
	data = appendRouteAttribute(data, rtaDPort, portBytes[:])
	binary.NativeEndian.PutUint32(data[0:4], uint32(len(data)))
	binary.NativeEndian.PutUint16(data[4:6], syscall.RTM_GETROUTE)
	binary.NativeEndian.PutUint16(data[6:8], syscall.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(data[8:12], seq)
	return data
}

func appendRouteAttribute(message []byte, attrType uint16, value []byte) []byte {
	const attrHeaderLen = 4
	length := attrHeaderLen + len(value)
	offset := len(message)
	message = append(message, make([]byte, align4(length))...)
	binary.NativeEndian.PutUint16(message[offset:offset+2], uint16(length))
	binary.NativeEndian.PutUint16(message[offset+2:offset+4], attrType)
	copy(message[offset+attrHeaderLen:], value)
	return message
}

func parseResolvedRoute(message syscall.NetlinkMessage) (routeResult, error) {
	const (
		netlinkAttributeTypeMask = 0x3fff
		rtaNextHopID             = 30
	)
	if len(message.Data) < syscall.SizeofRtMsg {
		return routeResult{}, errors.New("short resolved route")
	}
	routeType := message.Data[7]
	if routeType != syscall.RTN_UNICAST {
		return routeResult{}, ErrNoUsableRoute
	}
	attrs, err := syscall.ParseNetlinkRouteAttr(&message)
	if err != nil {
		return routeResult{}, err
	}
	result := routeResult{}
	oifCount := 0
	for _, attr := range attrs {
		switch attr.Attr.Type & netlinkAttributeTypeMask {
		case syscall.RTA_OIF:
			if len(attr.Value) != 4 {
				return routeResult{}, errors.New("resolved route has invalid output interface")
			}
			oifCount++
			result.InterfaceIndex = int(binary.NativeEndian.Uint32(attr.Value))
		case syscall.RTA_MULTIPATH:
			result.Multipath = true
		case rtaNextHopID:
			result.PolicyUnclear = true
		}
	}
	if oifCount != 1 {
		result.PolicyUnclear = true
	}
	return result, nil
}

func align4(value int) int { return (value + 3) &^ 3 }
