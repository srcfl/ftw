// ftw-driver-repository builds, signs, and verifies the independently
// published Lua-driver catalog sourced from this monorepo.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/driverrepo"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "publish":
		err = publish(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	case "keygen":
		err = keygen(os.Args[2:])
	case "check-versions":
		err = checkVersions(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ftw-driver-repository:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ftw-driver-repository <publish|verify|keygen|check-versions> [flags]")
}

func publish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	driversDir := fs.String("drivers", "../drivers", "source drivers directory")
	outputDir := fs.String("output", "../dist/driver-repository", "generated publication directory")
	baseURL := fs.String("base-url", "", "HTTPS release-asset base URL")
	repository := fs.String("repository", "https://github.com/srcfl/ftw", "source repository URL")
	commit := fs.String("commit", os.Getenv("GITHUB_SHA"), "source commit")
	keyID := fs.String("key-id", envOr("FTW_DRIVER_KEY_ID", "ftw-drivers-2026-01"), "manifest signing key ID")
	privateKeyEnv := fs.String("private-key-env", "FTW_DRIVER_SIGNING_KEY", "environment variable containing the base64 private key")
	publicKeyEnv := fs.String("expected-public-key-env", "FTW_DRIVER_SIGNING_PUBLIC_KEY", "optional environment variable containing the pinned public key")
	unsigned := fs.Bool("unsigned", false, "build an unsigned local validation manifest")
	generatedAt := fs.String("generated-at", "", "RFC3339 timestamp (defaults to now)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var at time.Time
	var err error
	if *generatedAt != "" {
		at, err = time.Parse(time.RFC3339, *generatedAt)
		if err != nil {
			return fmt.Errorf("parse generated-at: %w", err)
		}
	}
	var private ed25519.PrivateKey
	var expectedPublic ed25519.PublicKey
	if !*unsigned {
		private, err = driverrepo.DecodePrivateKey(os.Getenv(*privateKeyEnv))
		if err != nil {
			return fmt.Errorf("%s: %w", *privateKeyEnv, err)
		}
		if encoded := os.Getenv(*publicKeyEnv); encoded != "" {
			expectedPublic, err = driverrepo.DecodePublicKey(encoded)
			if err != nil {
				return fmt.Errorf("%s: %w", *publicKeyEnv, err)
			}
		}
	}
	publication, err := driverrepo.BuildPublication(driverrepo.PublicationOptions{
		DriversDir: *driversDir, OutputDir: *outputDir, BaseURL: *baseURL,
		Repository: *repository, Commit: *commit, KeyID: *keyID,
		PrivateKey: private, ExpectedPublicKey: expectedPublic, GeneratedAt: at, Unsigned: *unsigned,
	})
	if err != nil {
		return err
	}
	fmt.Printf("published %d drivers to %s\n", len(publication.Manifest.Drivers), *outputDir)
	if len(publication.PublicKey) > 0 {
		fmt.Printf("signing key id: %s\n", *keyID)
	}
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "../dist/driver-repository/manifest.json", "signed manifest path")
	publicationDir := fs.String("artifacts", "../dist/driver-repository", "local release-artifact directory")
	keyID := fs.String("key-id", envOr("FTW_DRIVER_KEY_ID", "ftw-drivers-2026-01"), "trusted signing key ID")
	publicKeyEnv := fs.String("public-key-env", "FTW_DRIVER_SIGNING_PUBLIC_KEY", "environment variable containing the base64 public key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	manifest, err := driverrepo.VerifyPublication(raw, *keyID, os.Getenv(*publicKeyEnv))
	if err != nil {
		return err
	}
	if err := driverrepo.VerifyPublicationArtifacts(manifest, *publicationDir); err != nil {
		return err
	}
	fmt.Printf("verified %d signed drivers from %s\n", len(manifest.Drivers), *manifestPath)
	return nil
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	privateOut := fs.String("private-out", "", "new private-key file (base64, mode 0600)")
	publicOut := fs.String("public-out", "", "new public-key file (base64, mode 0644)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *privateOut == "" || *publicOut == "" {
		return fmt.Errorf("private-out and public-out are required")
	}
	if filepath.Clean(*privateOut) == filepath.Clean(*publicOut) {
		return fmt.Errorf("private and public output paths must differ")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writeNewSecret(*privateOut, base64.StdEncoding.EncodeToString(private)+"\n", 0o600); err != nil {
		return err
	}
	if err := writeNewSecret(*publicOut, base64.StdEncoding.EncodeToString(public)+"\n", 0o644); err != nil {
		_ = os.Remove(*privateOut)
		return err
	}
	fmt.Printf("created private key at %s and public key at %s\n", *privateOut, *publicOut)
	return nil
}

func checkVersions(args []string) error {
	fs := flag.NewFlagSet("check-versions", flag.ContinueOnError)
	repoRoot := fs.String("repo-root", "..", "Git repository root")
	base := fs.String("base", "", "base Git revision")
	head := fs.String("head", "HEAD", "head Git revision or WORKTREE")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *base == "" {
		return errors.New("base revision is required")
	}
	diffRange := *base + "..." + *head
	diffArgs := []string{"diff", "--name-status", "--find-renames", diffRange, "--", "drivers"}
	if *head == "WORKTREE" {
		diffArgs = []string{"diff", "--name-status", "--find-renames", *base, "--", "drivers"}
	}
	output, err := gitOutput(*repoRoot, diffArgs...)
	if err != nil {
		return err
	}
	var failures []error
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return fmt.Errorf("unexpected git diff line %q", line)
		}
		status := fields[0]
		if strings.HasPrefix(status, "D") {
			continue
		}
		oldPath, newPath := fields[1], fields[1]
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if len(fields) < 3 {
				return fmt.Errorf("unexpected rename line %q", line)
			}
			oldPath, newPath = fields[1], fields[2]
		}
		if filepath.Ext(newPath) != ".lua" {
			continue
		}
		var previous []byte
		if !strings.HasPrefix(status, "A") {
			previous, err = gitOutput(*repoRoot, "show", *base+":"+oldPath)
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: read base revision: %w", oldPath, err))
				continue
			}
		}
		var current []byte
		if *head == "WORKTREE" {
			current, err = os.ReadFile(filepath.Join(*repoRoot, filepath.FromSlash(newPath)))
		} else {
			current, err = gitOutput(*repoRoot, "show", *head+":"+newPath)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: read current revision: %w", newPath, err))
			continue
		}
		if err := driverrepo.ValidateDriverVersionChange(newPath, previous, current); err != nil {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	fmt.Println("driver version changes verified")
	return nil
}

func gitOutput(repoRoot string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", repoRoot}, args...)
	output, err := exec.Command("git", commandArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func writeNewSecret(path, value string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
