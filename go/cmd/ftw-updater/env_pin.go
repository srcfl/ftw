package main

// Persisting the tags an update installed.
//
// Compose resolves `image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}` fresh on
// every invocation. The updater passes FTW_IMAGE_TAG for its own `compose up`,
// but nothing records it, so the next plain `docker compose up -d` on the host
// — a reboot, adding a service, any routine maintenance — resolves `:latest`
// again and silently moves the site back to newest stable. A beta tester loses
// their beta without touching anything.
//
// The fix is to write the installed tags into the project's .env, which is
// where compose already looks. The updater mounts the project read-only on
// purpose, so it composes the new file content and the detached helper writes
// it: that container already holds the Docker socket, which is strictly more
// power than writing one file next to the compose file it is about to run.

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func base64Encode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// envAssignment matches a KEY=value line, ignoring leading whitespace and an
// optional `export `. Comments and blank lines are preserved verbatim.
func envAssignmentKey(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")
	eq := strings.IndexByte(trimmed, '=')
	if eq <= 0 {
		return "", false
	}
	key := strings.TrimSpace(trimmed[:eq])
	if key == "" {
		return "", false
	}
	return key, true
}

// mergeEnvFile returns the contents of path with tags applied: existing keys
// are rewritten in place, missing ones appended. Every other line — comments,
// blanks, unrelated settings — survives byte for byte, because this file
// belongs to the operator and an update is not a licence to reformat it.
func mergeEnvFile(existing string, tags map[string]string) string {
	remaining := make(map[string]string, len(tags))
	for k, v := range tags {
		remaining[k] = v
	}

	var out []string
	if existing != "" {
		for _, line := range strings.Split(strings.TrimSuffix(existing, "\n"), "\n") {
			if key, ok := envAssignmentKey(line); ok {
				if value, wanted := remaining[key]; wanted {
					out = append(out, key+"="+value)
					delete(remaining, key)
					continue
				}
			}
			out = append(out, line)
		}
	}

	// Appending in a fixed order keeps the file stable across runs; map order
	// would otherwise reshuffle it and make every update look like a change.
	for _, key := range []string{mainTagEnv, updaterTagEnv} {
		if value, ok := remaining[key]; ok {
			out = append(out, key+"="+value)
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// envPinScript returns a shell fragment that writes content to <dir>/.env
// atomically. The payload is base64 so no quoting in it can break out of the
// helper's `sh -c`, and the temp file is created in the same directory so the
// rename cannot cross a filesystem boundary.
func envPinScript(dir, content string) string {
	encoded := base64Encode(content)
	envPath := filepath.Join(dir, ".env")
	tmpPath := envPath + ".ftw-update-tmp"
	return fmt.Sprintf(
		"printf %%s '%s' | base64 -d > '%s' && mv '%s' '%s' || echo 'ftw: could not persist image tags to %s' >&2",
		encoded, tmpPath, tmpPath, envPath, envPath,
	)
}

// readEnvFile returns the current .env contents, or empty when there is none.
// A read error is reported so the caller can skip the rewrite rather than
// overwrite a file it could not see.
func readEnvFile(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}
