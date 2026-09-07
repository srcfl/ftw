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

	"gopkg.in/yaml.v3"
)

const releaseIdentityOverrideName = "docker-compose.override.yml"

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
		lines := strings.Split(strings.TrimSuffix(existing, "\n"), "\n")
		lastAssignment := make(map[string]int, len(tags))
		for i, line := range lines {
			if key, ok := envAssignmentKey(line); ok {
				if _, wanted := tags[key]; wanted {
					lastAssignment[key] = i
				}
			}
		}
		for i, line := range lines {
			if key, ok := envAssignmentKey(line); ok {
				if value, wanted := remaining[key]; wanted {
					// Compose and dotenv readers use the last assignment. Remove
					// older duplicates and replace the effective one in place.
					if lastAssignment[key] != i {
						continue
					}
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

// shellQuote returns one POSIX sh word. Project paths are operator-controlled
// and may contain spaces or apostrophes, while the helper command runs via
// `sh -c` inside a detached container.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// envPinScript returns a shell fragment that writes content to <dir>/.env
// atomically. The payload is base64 so no quoting in it can break out of the
// helper's `sh -c`, and the temp file is created in the same directory so the
// rename cannot cross a filesystem boundary.
func envPinScript(dir, content string) string {
	envPath := filepath.Join(dir, ".env")
	tmpPath := envPath + ".ftw-update-tmp"
	return fmt.Sprintf(
		"%s && mv %s %s || printf '%%s\\n' %s >&2",
		envTempWriteScript(envPath, tmpPath, content), shellQuote(tmpPath), shellQuote(envPath),
		shellQuote("ftw: could not persist image tags to "+envPath),
	)
}

// envTempWriteScript seeds the temporary file with the current .env metadata
// before replacing its contents. cp -p keeps the operator's owner and mode;
// a new file starts private because .env can hold API tokens.
func envTempWriteScript(envPath, tmpPath, content string) string {
	return fmt.Sprintf(
		"{ if [ -e %s ]; then cp -p %s %s; else rm -f %s && : > %s && chmod 600 %s; fi; } && printf %%s %s | base64 -d > %s",
		shellQuote(envPath), shellQuote(envPath), shellQuote(tmpPath), shellQuote(tmpPath),
		shellQuote(tmpPath), shellQuote(tmpPath), shellQuote(base64Encode(content)), shellQuote(tmpPath),
	)
}

// releaseIdentityPinStep records the selected Core tag and, for the supported
// legacy layout, the matching container environment mapping. A plain Compose
// recreate then gives Core the same release identity as the image it starts.
func (s *server) releaseIdentityPinStep(dir, target string) (string, error) {
	image, ok, err := serviceImageFromComposeFiles(s.hostComposeFiles(), s.mainServiceName)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("service %s has no host Compose image", s.mainServiceName)
	}
	if _, ok := composeImageRepositoryForTag(image, mainTagEnv); !ok {
		return "", fmt.Errorf("service %s host image does not persist %s", s.mainServiceName, mainTagEnv)
	}

	existing, err := readEnvFile(dir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Join(dir, ".env"), err)
	}
	envContent := mergeEnvFile(existing, map[string]string{
		mainTagEnv:    target,
		updaterTagEnv: target,
	})

	mapped, err := serviceEnvironmentUsesVariable(s.hostComposeFiles(), s.mainServiceName, mainTagEnv, mainTagEnv)
	if err != nil {
		return "", err
	}
	if mapped {
		return envPinScript(dir, envContent), nil
	}

	// A custom override belongs to the operator. The migration path already
	// fails closed for one that omits this mapping, so do not rewrite it here.
	if len(s.overrideFiles) != 0 {
		return "", fmt.Errorf("existing Compose override does not pass %s into %s", mainTagEnv, s.mainServiceName)
	}
	if filepath.Base(s.composeFile) != "docker-compose.yml" {
		return "", fmt.Errorf("cannot add the standard release identity override beside %s", filepath.Base(s.composeFile))
	}

	overridePath := filepath.Join(dir, releaseIdentityOverrideName)
	if _, err := os.Lstat(overridePath); err == nil {
		return "", fmt.Errorf("%s appeared after updater startup; refusing to overwrite it", overridePath)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect %s: %w", overridePath, err)
	}
	overrideContent := fmt.Sprintf("services:\n  %s:\n    environment:\n      %s: ${%s:-}\n", s.mainServiceName, mainTagEnv, mainTagEnv)
	return newOverrideAndEnvPinScript(overridePath, overrideContent, filepath.Join(dir, ".env"), envContent), nil
}

// newOverrideAndEnvPinScript creates a new standard Compose override before
// moving the .env pin into place. The hard link makes the no-overwrite check
// atomic: if an operator creates the override first, neither file is changed.
func newOverrideAndEnvPinScript(overridePath, overrideContent, envPath, envContent string) string {
	overrideTmp := overridePath + ".ftw-update-tmp"
	envTmp := envPath + ".ftw-update-tmp"
	return fmt.Sprintf(
		"{ printf %%s %s | base64 -d > %s && ln %s %s && rm -f %s && %s && mv %s %s; } || { rm -f %s %s; printf '%%s\\n' %s >&2; }",
		shellQuote(base64Encode(overrideContent)), shellQuote(overrideTmp), shellQuote(overrideTmp),
		shellQuote(overridePath), shellQuote(overrideTmp), envTempWriteScript(envPath, envTmp, envContent),
		shellQuote(envTmp), shellQuote(envPath), shellQuote(overrideTmp), shellQuote(envTmp),
		shellQuote("ftw: could not persist release identity beside "+overridePath),
	)
}

func serviceEnvironmentUsesVariable(files []string, service, key, variable string) (bool, error) {
	value := ""
	found := false
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", path, err)
		}
		var doc struct {
			Services map[string]yaml.Node `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
		node, ok := doc.Services[service]
		if !ok {
			continue
		}
		var svc struct {
			Environment yaml.Node `yaml:"environment"`
		}
		if err := node.Decode(&svc); err != nil {
			return false, fmt.Errorf("parse %s service %s: %w", path, service, err)
		}
		entry, ok, err := composeEnvironmentValue(svc.Environment, key)
		if err != nil {
			return false, fmt.Errorf("parse %s service %s environment: %w", path, service, err)
		}
		if ok {
			value, found = entry, true
		}
	}
	return found && composeValueUsesVariable(value, variable), nil
}

// composeEnvironmentValue reads one Compose environment entry from either of
// the two forms the Compose file format accepts:
//
//	KEY: value
//	- KEY=value
//
// A bare list entry ("- KEY") asks Compose to copy a value from its own
// process environment. It is still a found override, but it does not prove
// that the variable is passed through by value, so callers receive an empty
// value and fail closed.
func composeEnvironmentValue(environment yaml.Node, key string) (string, bool, error) {
	switch environment.Kind {
	case 0:
		return "", false, nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(environment.Content); i += 2 {
			name := environment.Content[i]
			value := environment.Content[i+1]
			if name.Value != key {
				continue
			}
			if value.Kind != yaml.ScalarNode {
				return "", false, fmt.Errorf("%s value must be a scalar", key)
			}
			return value.Value, true, nil
		}
		return "", false, nil
	case yaml.SequenceNode:
		value := ""
		found := false
		for _, item := range environment.Content {
			if item.Kind != yaml.ScalarNode {
				return "", false, fmt.Errorf("list entry must be a scalar")
			}
			name, candidate, assigned := strings.Cut(item.Value, "=")
			if name != key {
				continue
			}
			found = true
			if !assigned {
				value = ""
				continue
			}
			value = candidate
		}
		return value, found, nil
	default:
		return "", false, fmt.Errorf("must be a mapping or list")
	}
}

func composeValueUsesVariable(value, variable string) bool {
	if value == "$"+variable || value == "${"+variable+"}" {
		return true
	}
	prefix := "${" + variable
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, "}") {
		return false
	}
	inner := value[len(prefix) : len(value)-1]
	var fallback string
	switch {
	case strings.HasPrefix(inner, ":-"):
		fallback = strings.TrimPrefix(inner, ":-")
	case strings.HasPrefix(inner, "-"):
		fallback = strings.TrimPrefix(inner, "-")
	default:
		return false
	}
	// The whole value must be one expansion. Nested or adjacent expansions
	// could add text to the installed tag and are not an exact pass-through.
	return !strings.ContainsAny(fallback, "${}")
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
