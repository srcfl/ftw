//go:build windows

package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func verifyConfigFileOwnerOnly(path string) error {
	path, err := normalizeWindowsConfigPath(path)
	if err != nil {
		return fmt.Errorf("normalize config path: %w", err)
	}
	ownerSID, err := currentProcessUserSID()
	if err != nil {
		return fmt.Errorf("read current user SID: %w", err)
	}
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		configSecurityQuery,
	)
	if err != nil {
		return fmt.Errorf("read config security descriptor: %w", err)
	}
	return validateOwnerOnlyConfigSecurityDescriptor(sd, ownerSID)
}

func TestConfigSecurityQueryRequestsOnlyReadableParts(t *testing.T) {
	want := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if configSecurityQuery != want {
		t.Fatalf("config security query = %#x, want owner and DACL only (%#x)", configSecurityQuery, want)
	}
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
	if n, err := f.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("config temp handle lacks network-safe read access: read = %d, %v", n, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateOwnerOnlyConfigSecurityDescriptorFailsClosed(t *testing.T) {
	ownerSID, err := currentProcessUserSID()
	if err != nil {
		t.Fatal(err)
	}
	valid, err := ownerOnlyConfigSecurityDescriptor(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOwnerOnlyConfigSecurityDescriptor(valid, ownerSID); err != nil {
		t.Fatalf("owner-only descriptor was rejected: %v", err)
	}

	missingDACL, err := windows.NewSecurityDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err := missingDACL.SetOwner(ownerSID, false); err != nil {
		t.Fatal(err)
	}
	if err := missingDACL.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		t.Fatal(err)
	}

	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	worldACL, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(worldSID),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	permissive, err := windows.NewSecurityDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err := permissive.SetOwner(ownerSID, false); err != nil {
		t.Fatal(err)
	}
	if err := permissive.SetDACL(worldACL, true, false); err != nil {
		t.Fatal(err)
	}
	if err := permissive.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		t.Fatal(err)
	}

	for name, sd := range map[string]*windows.SECURITY_DESCRIPTOR{
		"missing DACL":    missingDACL,
		"permissive DACL": permissive,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateOwnerOnlyConfigSecurityDescriptor(sd, ownerSID); err == nil {
				t.Fatal("unsafe security descriptor was accepted")
			}
		})
	}
}

func TestCreateConfigTempDoesNotFollowReparsePoint(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target-that-must-not-be-created")
	link := filepath.Join(dir, "config.yaml.tmp")
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	linkPtr, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	const symbolicLinkFlagAllowUnprivilegedCreate = 0x2
	if err := windows.CreateSymbolicLink(linkPtr, targetPtr, symbolicLinkFlagAllowUnprivilegedCreate); err != nil {
		t.Fatalf("CreateSymbolicLink with the unprivileged flag failed; hosted Windows must support this test: %v", err)
	}
	defer os.Remove(link)

	f, err := createConfigTemp(link, configFileMode)
	if err == nil {
		_ = f.Close()
		t.Fatal("createConfigTemp opened a reparse point")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reparse target status = %v, target must not be created or written", err)
	}
	if got, err := os.Readlink(link); err != nil {
		t.Fatalf("reparse candidate was not preserved: %v", err)
	} else if got != target {
		t.Fatalf("reparse target = %q, want %q", got, target)
	}
}

func TestSaveAtomicSupportsLongWindowsPath(t *testing.T) {
	dir := t.TempDir()
	for len(filepath.Join(dir, "config.yaml")) <= 260 {
		dir = filepath.Join(dir, "long-config-path-"+strings.Repeat("x", 20))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.yaml")
	if len(path) <= 260 {
		t.Fatalf("test path length = %d, want more than MAX_PATH", len(path))
	}
	normalized, err := normalizeWindowsConfigPath(path + ".tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(normalized, `\\?\`) {
		t.Fatalf("normalized long path = %q, want extended prefix", normalized)
	}
	c, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	if err := verifyConfigFileOwnerOnly(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatalf("saved config at long path cannot be read: %v", err)
	}
}

func TestNormalizeWindowsConfigPathKeepsShortPathsAndPrefixesLongUNC(t *testing.T) {
	short := filepath.Join(t.TempDir(), "config.yaml")
	got, err := normalizeWindowsConfigPath(short)
	if err != nil {
		t.Fatal(err)
	}
	if got != short {
		t.Fatalf("short path changed from %q to %q", short, got)
	}
	relative, err := normalizeWindowsConfigPath("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if relative != "config.yaml" {
		t.Fatalf("short relative path changed to %q", relative)
	}
	unc := `\\server\share\` + strings.Repeat("x", 240) + `\config.yaml`
	got, err = normalizeWindowsConfigPath(unc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, `\\?\UNC\server\share\`) {
		t.Fatalf("long UNC path = %q, want \\?\\UNC prefix", got)
	}
}

func TestSettingsDatabaseFilesHaveOwnerOnlyACL(t *testing.T) {
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm"} {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, nil, 0666); err != nil {
			t.Fatal(err)
		}
		if err := restrictConfigFile(path); err != nil {
			t.Fatal(err)
		}
		if err := verifyConfigFileOwnerOnly(path); err != nil {
			t.Fatal(err)
		}
	}
}
