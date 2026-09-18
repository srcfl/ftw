//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createConfigTemp passes a protected security descriptor to CreateFile. The
// descriptor must be present at creation time: applying an ACL after
// os.OpenFile would leave a race in which another local user could read the
// credentials in the temp file before the ACL was tightened.
func createConfigTemp(path string, _ os.FileMode) (*os.File, error) {
	ownerSID, err := currentProcessUserSID()
	if err != nil {
		return nil, fmt.Errorf("read config owner SID: %w", err)
	}
	sd, err := ownerOnlyConfigSecurityDescriptor(ownerSID)
	if err != nil {
		return nil, fmt.Errorf("build private config security descriptor: %w", err)
	}
	openPath, err := normalizeWindowsConfigPath(path)
	if err != nil {
		return nil, fmt.Errorf("normalize config temp path: %w", err)
	}
	pathPtr, err := windows.UTF16PtrFromString(openPath)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	h, err := windows.CreateFile(
		pathPtr,
		// Microsoft recommends read+write for network files; write-only handles
		// can intermittently fail with ERROR_ACCESS_DENIED over SMB.
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		sa,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	if err := verifyOwnerOnlyConfigHandle(h, ownerSID); err != nil {
		cleanupErrs := []error{fmt.Errorf("verify private config ACL before write: %w", err)}
		if err := windows.CloseHandle(h); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("close rejected config temp: %w", err))
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove rejected config temp: %w", err))
		}
		return nil, errors.Join(cleanupErrs...)
	}
	f := os.NewFile(uintptr(h), path)
	if f == nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("wrap config temp handle")
	}
	return f, nil
}

const windowsLongPathThreshold = 248

// normalizeWindowsConfigPath follows the long-path rule used by the Go
// standard library. Short and relative paths stay unchanged. A long path is
// first made absolute, then gets the extended prefix needed by CreateFile;
// UNC paths use the required \\?\UNC form.
func normalizeWindowsConfigPath(path string) (string, error) {
	if isExtendedWindowsPath(path) {
		return path, nil
	}
	pathLength := len(path)
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		pathLength += len(cwd) + 1
	}
	if pathLength < windowsLongPathThreshold {
		return path, nil
	}
	fullPath, err := fullWindowsPath(path)
	if err != nil {
		return "", err
	}
	if isExtendedWindowsPath(fullPath) || isWindowsDevicePath(fullPath) {
		return fullPath, nil
	}
	if isWindowsUNCPath(fullPath) {
		return `\\?\UNC\` + fullPath[2:], nil
	}
	return `\\?\` + fullPath, nil
}

func fullWindowsPath(path string) (string, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	size := uint32(len(path) + 1)
	if size < 256 {
		size = 256
	}
	for {
		buf := make([]uint16, size)
		n, err := windows.GetFullPathName(pathPtr, size, &buf[0], nil)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", fmt.Errorf("GetFullPathName returned an empty path")
		}
		if n < size {
			return windows.UTF16ToString(buf[:n]), nil
		}
		size = n + 1
	}
}

func isExtendedWindowsPath(path string) bool {
	if len(path) < 4 {
		return false
	}
	return path[:4] == `\??\` ||
		(isWindowsSeparator(path[0]) && isWindowsSeparator(path[1]) && path[2] == '?' && isWindowsSeparator(path[3]))
}

func isWindowsUNCPath(path string) bool {
	return len(path) >= 2 && isWindowsSeparator(path[0]) && isWindowsSeparator(path[1])
}

func isWindowsDevicePath(path string) bool {
	return len(path) >= 4 && isWindowsUNCPath(path) && path[2] == '.' && isWindowsSeparator(path[3])
}

func isWindowsSeparator(c byte) bool {
	return c == '\\' || c == '/'
}

func currentProcessUserSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("open current process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current user SID: %w", err)
	}
	ownerSID, err := user.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("copy current user SID: %w", err)
	}
	return ownerSID, nil
}

func ownerOnlyConfigSecurityDescriptor(ownerSID *windows.SID) (*windows.SECURITY_DESCRIPTOR, error) {
	if ownerSID == nil {
		return nil, fmt.Errorf("owner SID is nil")
	}

	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(ownerSID),
		},
	}}, nil)
	if err != nil {
		return nil, fmt.Errorf("build owner ACL: %w", err)
	}
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("initialize security descriptor: %w", err)
	}
	if err := sd.SetOwner(ownerSID, false); err != nil {
		return nil, fmt.Errorf("set security descriptor owner: %w", err)
	}
	if err := sd.SetDACL(acl, true, false); err != nil {
		return nil, fmt.Errorf("set security descriptor DACL: %w", err)
	}
	// Do not let an inheritable ACE from the config directory re-open the
	// file to another local account after CreateFile applies this descriptor.
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, fmt.Errorf("protect security descriptor DACL: %w", err)
	}
	return sd.ToSelfRelative()
}

func verifyOwnerOnlyConfigHandle(h windows.Handle, ownerSID *windows.SID) error {
	sd, err := windows.GetSecurityInfo(
		h,
		windows.SE_FILE_OBJECT,
		configSecurityQuery,
	)
	if err != nil {
		return fmt.Errorf("read created config security descriptor: %w", err)
	}
	return validateOwnerOnlyConfigSecurityDescriptor(sd, ownerSID)
}

const configSecurityQuery = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION

const windowsFileAllAccess windows.ACCESS_MASK = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

// validateOwnerOnlyConfigSecurityDescriptor checks the descriptor that the
// filesystem actually stored. CreateFile can succeed on filesystems that do
// not enforce the descriptor supplied at creation time, so this check must run
// before config bytes are written.
func validateOwnerOnlyConfigSecurityDescriptor(sd *windows.SECURITY_DESCRIPTOR, ownerSID *windows.SID) error {
	if sd == nil {
		return fmt.Errorf("security descriptor is nil")
	}
	if ownerSID == nil {
		return fmt.Errorf("expected owner SID is nil")
	}
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("read security descriptor control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("config DACL is not protected: control=%#x", control)
	}
	owner, defaulted, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read config owner: %w", err)
	}
	if owner == nil || !owner.Equals(ownerSID) {
		return fmt.Errorf("config owner does not match current user")
	}
	if defaulted {
		return fmt.Errorf("config owner is defaulted")
	}
	dacl, defaulted, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read config DACL: %w", err)
	}
	if dacl == nil {
		return fmt.Errorf("config DACL is nil")
	}
	if defaulted {
		return fmt.Errorf("config DACL is defaulted")
	}
	if dacl.AceCount != 1 {
		return fmt.Errorf("config DACL ACE count = %d, want one owner ACE", dacl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return fmt.Errorf("read config DACL ACE: %w", err)
	}
	if ace == nil {
		return fmt.Errorf("config owner ACE is nil")
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return fmt.Errorf("config DACL ACE type = %d, want allow", ace.Header.AceType)
	}
	if ace.Header.AceFlags != windows.NO_INHERITANCE {
		return fmt.Errorf("config owner ACE flags = %#x, want no inheritance", ace.Header.AceFlags)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(ownerSID) {
		return fmt.Errorf("config DACL ACE does not belong to current user")
	}
	if ace.Mask != windows.ACCESS_MASK(windows.GENERIC_ALL) && ace.Mask != windowsFileAllAccess {
		return fmt.Errorf("config owner ACE mask = %#x, want full control", ace.Mask)
	}
	return nil
}

func replaceConfigTemp(tmp, path string) error {
	// os.Rename uses MoveFileEx with REPLACE_EXISTING on Windows. Keep Go's
	// long-path handling and the existing atomic replacement semantics; the
	// temp file's protected DACL becomes the destination file's DACL.
	return os.Rename(tmp, path)
}

// syncDir fsyncs a directory where Windows permits it. Windows filesystems
// may reject directory FlushFileBuffers, so retain #792's best-effort rule.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return nil
	}
	return nil
}

// Existing SQLite files must receive the same protected ACL as the old seed
// before a settings transaction writes credentials into the WAL.
func restrictConfigFile(path string) error {
	owner, err := currentProcessUserSID()
	if err != nil {
		return err
	}
	sd, err := ownerOnlyConfigSecurityDescriptor(owner)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	path, err = normalizeWindowsConfigPath(path)
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil); err != nil {
		return err
	}
	actual, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, configSecurityQuery)
	if err != nil {
		return err
	}
	return validateOwnerOnlyConfigSecurityDescriptor(actual, owner)
}
