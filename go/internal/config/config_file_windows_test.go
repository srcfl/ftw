//go:build windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func verifyConfigFileOwnerOnly(path string) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return fmt.Errorf("open current process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current user SID: %w", err)
	}
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("read config security descriptor: %w", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("read security descriptor control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("config DACL is inheritable: control=%#x", control)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read config owner: %w", err)
	}
	if !owner.Equals(user.User.Sid) {
		return fmt.Errorf("config owner SID = %s, want current user %s", owner.String(), user.User.Sid.String())
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read config DACL: %w", err)
	}
	if dacl.AceCount != 1 {
		return fmt.Errorf("config DACL ACE count = %d, want one owner ACE", dacl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return fmt.Errorf("read config DACL ACE: %w", err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return fmt.Errorf("config DACL ACE type = %d, want allow", ace.Header.AceType)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(user.User.Sid) {
		return fmt.Errorf("config DACL ACE SID = %s, want current user %s", aceSID.String(), user.User.Sid.String())
	}
	required := uint32(windows.FILE_READ_DATA | windows.FILE_WRITE_DATA)
	if uint32(ace.Mask)&required != required {
		return fmt.Errorf("config owner ACE mask = %#x, lacks read/write", ace.Mask)
	}
	return nil
}

func TestCreateConfigTempUsesOwnerOnlyACLBeforeFirstWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.tmp")
	f, err := createConfigTemp(path, configFileMode)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if err := verifyConfigFileOwnerOnly(path); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
