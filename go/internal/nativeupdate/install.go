package nativeupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	receiptFile        = ".ftw-release.json"
	packageVersionFile = "release-version.json"
	maxArchiveBytes    = 1 << 30
	maxExtractBytes    = 2 << 30
)

type releaseReceipt struct {
	Tag           string `json:"tag"`
	Arch          string `json:"arch"`
	ArchiveSHA256 string `json:"archive_sha256"`
	StateSchema   int    `json:"state_schema"`
}

type packageVersion struct {
	Version     string `json:"version"`
	Arch        string `json:"arch"`
	StateSchema int    `json:"state_schema"`
}

func checkReceipt(dir, tag, arch string) error {
	_, err := readReceipt(dir, tag, arch)
	return err
}

func readReceipt(dir, tag, arch string) (releaseReceipt, error) {
	data, err := os.ReadFile(filepath.Join(dir, receiptFile))
	if err != nil {
		return releaseReceipt{}, fmt.Errorf("release %s has no verified receipt: %w", tag, err)
	}
	var receipt releaseReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return releaseReceipt{}, err
	}
	if receipt.Tag != tag || receipt.Arch != arch || receipt.StateSchema <= 0 || !validDigest(receipt.ArchiveSHA256) {
		return releaseReceipt{}, fmt.Errorf("release %s has an invalid receipt", tag)
	}
	return receipt, nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

// InstallArchive accepts one immutable release package and its matching
// checksum. A complete release appears in releases/ only after verification.
func (m Manager) InstallArchive(ctx context.Context, tag, archivePath, checksumPath string) error {
	if !ValidTag(tag) {
		return fmt.Errorf("invalid release tag %q", tag)
	}
	arch := runtime.GOARCH
	if arch != "arm64" && arch != "amd64" {
		return fmt.Errorf("unsupported host architecture %s", arch)
	}
	name := "ftw-linux-" + arch + ".tar.gz"
	digest, err := readChecksum(checksumPath, name)
	if err != nil {
		return err
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(archive, maxArchiveBytes+1))
	if err != nil {
		return err
	}
	if n > maxArchiveBytes || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("release archive size or checksum mismatch")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	releases := filepath.Join(m.Root, "releases")
	if err := os.MkdirAll(releases, 0o755); err != nil {
		return err
	}
	target := filepath.Join(releases, tag)
	if _, err := os.Lstat(target); err == nil {
		if err := checkReceipt(target, tag, arch); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(target, receiptFile))
		if err != nil {
			return err
		}
		var prior releaseReceipt
		if err := json.Unmarshal(data, &prior); err != nil || prior.ArchiveSHA256 != digest {
			return fmt.Errorf("release %s already exists with different content", tag)
		}
		_, err = m.ReleaseDir(tag)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(releases, ".stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := extractArchive(ctx, archive, stage); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(stage, packageVersionFile))
	if err != nil {
		return fmt.Errorf("release has no version metadata: %w", err)
	}
	var version packageVersion
	if err := json.Unmarshal(data, &version); err != nil {
		return err
	}
	if version.Version != tag || version.Arch != arch || version.StateSchema <= 0 {
		return fmt.Errorf("release identity mismatch: package %s/%s, requested %s/%s", version.Version, version.Arch, tag, arch)
	}
	receipt, _ := json.Marshal(releaseReceipt{Tag: tag, Arch: arch, ArchiveSHA256: digest, StateSchema: version.StateSchema})
	if err := os.WriteFile(filepath.Join(stage, receiptFile), append(receipt, '\n'), 0o600); err != nil {
		return err
	}
	if err := validateReleasePath(stage, tag, arch); err != nil {
		return err
	}
	if err := syncDir(stage); err != nil {
		return err
	}
	return m.locked(func() error {
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("release %s appeared during staging", tag)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(stage, target); err != nil {
			return err
		}
		return syncDir(releases)
	})
}

func readChecksum(path, name string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.TrimSuffix(string(data), "\n"), "  ")
	if len(parts) != 2 || parts[1] != name || !validDigest(parts[0]) {
		return "", errors.New("release checksum does not name the expected archive")
	}
	return parts[0], nil
}

func extractArchive(ctx context.Context, source io.Reader, target string) error {
	gz, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	seen := make(map[string]bool)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !fs.ValidPath(name) || name == "." || name == receiptFile || seen[name] || len(seen) >= 20000 {
			return fmt.Errorf("unsafe or duplicate release entry %q", header.Name)
		}
		seen[name] = true
		if header.Typeflag == tar.TypeSymlink && name == "forty-two-watts" && header.Linkname == "ftw" {
			continue // Legacy download alias is not needed by the launcher.
		}
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("unsafe release entry type for %q", name)
		}
		path := filepath.Join(target, filepath.FromSlash(name))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if header.Size < 0 || header.Size > maxExtractBytes-total {
			return errors.New("release exceeds extracted size limit")
		}
		total += header.Size
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if header.Mode&0o111 != 0 {
			mode = 0o755
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(file, tarReader, header.Size)
		if copyErr == nil {
			copyErr = file.Sync()
		}
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
