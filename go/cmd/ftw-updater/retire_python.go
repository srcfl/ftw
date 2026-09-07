package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// retiredPythonCompose removes only the old planner's service and IPC wiring.
// Preserve custom services, the Core data mount, image pins and YAML tags.
func retiredPythonCompose(data []byte) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("expected a Compose mapping")
	}
	var ambiguous func(*yaml.Node) bool
	ambiguous = func(n *yaml.Node) bool {
		if n.Kind == yaml.AliasNode || n.Tag == "!!merge" {
			return true
		}
		for _, child := range n.Content {
			if ambiguous(child) {
				return true
			}
		}
		return false
	}
	// Aliases can share nodes with custom services. Do not rewrite through them.
	if ambiguous(&doc) {
		return nil, false, fmt.Errorf("Compose YAML anchors or merges need manual retirement")
	}
	changed := false
	var prune func(*yaml.Node, func(string, *yaml.Node) bool)
	prune = func(n *yaml.Node, drop func(string, *yaml.Node) bool) {
		if n == nil || n.Kind != yaml.MappingNode {
			return
		}
		out := n.Content[:0]
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if drop(k.Value, v) {
				changed = true
				continue
			}
			out = append(out, k, v)
		}
		n.Content = out
	}
	root := doc.Content[0]
	prune(root, func(k string, v *yaml.Node) bool {
		if k == "services" {
			prune(v, func(name string, service *yaml.Node) bool {
				if name == "ftw-optimizer" {
					return true
				}
				if name != canonicalMainServiceName && name != legacyMainServiceName {
					return false
				}
				prune(service, func(field string, value *yaml.Node) bool {
					switch field {
					case "environment":
						if value.Kind == yaml.MappingNode {
							prune(value, func(key string, _ *yaml.Node) bool { return strings.HasPrefix(key, "FTW_OPTIMIZER_") })
						} else if value.Kind == yaml.SequenceNode {
							out := value.Content[:0]
							for _, item := range value.Content {
								if strings.HasPrefix(item.Value, "FTW_OPTIMIZER_") {
									changed = true
								} else {
									out = append(out, item)
								}
							}
							value.Content = out
						}
					case "volumes":
						if value.Kind == yaml.SequenceNode {
							out := value.Content[:0]
							for _, item := range value.Content {
								retired := false
								if item.Kind == yaml.ScalarNode {
									parts := strings.Split(item.Value, ":")
									retired = len(parts) > 1 && parts[1] == "/run/ftw-optimizer"
								} else if item.Kind == yaml.MappingNode {
									for j := 0; j < len(item.Content); j += 2 {
										if item.Content[j].Value == "target" && item.Content[j+1].Value == "/run/ftw-optimizer" {
											retired = true
										}
									}
								}
								if retired {
									changed = true
								} else {
									out = append(out, item)
								}
							}
							value.Content = out
						}
					case "depends_on":
						if value.Kind == yaml.MappingNode {
							prune(value, func(key string, _ *yaml.Node) bool { return key == "ftw-optimizer" })
						} else if value.Kind == yaml.SequenceNode {
							out := value.Content[:0]
							for _, item := range value.Content {
								if item.Value == "ftw-optimizer" {
									changed = true
								} else {
									out = append(out, item)
								}
							}
							value.Content = out
						}
					}
					return false
				})
				return false
			})
		}
		return false
	})
	var volumeUsed func(*yaml.Node) bool
	volumeUsed = func(n *yaml.Node) bool {
		if n.Kind == yaml.ScalarNode && (n.Value == "optimizer-ipc" || strings.HasPrefix(n.Value, "optimizer-ipc:")) {
			return true
		}
		for _, child := range n.Content {
			if volumeUsed(child) {
				return true
			}
		}
		return false
	}
	used := false
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "services" {
			used = volumeUsed(root.Content[i+1])
		}
	}
	if !used {
		prune(root, func(k string, v *yaml.Node) bool {
			if k == "volumes" {
				prune(v, func(name string, _ *yaml.Node) bool { return name == "optimizer-ipc" })
			}
			return false
		})
	}
	if !changed {
		return data, false, nil
	}
	var out bytes.Buffer
	e := yaml.NewEncoder(&out)
	e.SetIndent(2)
	if err := e.Encode(&doc); err != nil {
		return nil, false, err
	}
	return out.Bytes(), true, nil
}

// The running updater mounts Compose read-only. Use its exact local image in
// a short-lived helper with a writable project mount, as self-replacement does.
func (s *server) retirePythonViaHelper(ctx context.Context) error {
	image, err := s.imageID(ctx, "ftw-updater")
	if err != nil {
		return fmt.Errorf("current updater image: %w", err)
	}
	projectDir := filepath.Dir(s.composeFile)
	args := []string{"run", "--rm", "--pull", "never", "--network", "none",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", projectDir + ":" + projectDir + ":rw", "-w", projectDir,
		"-e", "FTW_RETIRE_PYTHON_HELPER=1"}
	if project := os.Getenv("COMPOSE_PROJECT_NAME"); project != "" {
		args = append(args, "-e", "COMPOSE_PROJECT_NAME="+project)
	}
	args = append(args, "--entrypoint", "/usr/local/bin/ftw-updater", image,
		"-retire-python", "-compose", s.composeFile, "-main-service", s.mainServiceName)
	return s.runner(ctx, nil, args...)
}

func (s *server) retiredPythonContainers(ctx context.Context) ([]string, error) {
	coreID, err := s.serviceContainerID(ctx, s.mainServiceName)
	if err != nil {
		return nil, err
	}
	label, err := exec.CommandContext(ctx, "docker", "inspect", "--format", `{{ index .Config.Labels "com.docker.compose.project" }}`, coreID).Output()
	if err != nil {
		return nil, err
	}
	project := strings.TrimSpace(string(label))
	if project == "" || project == "<no value>" {
		return nil, fmt.Errorf("Core has no Compose project label")
	}
	// Labels find orphaned containers too, without touching another project.
	out, err := exec.CommandContext(ctx, "docker", "ps", "--all", "--quiet",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service=ftw-optimizer").Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

func (s *server) retirePythonOptimizer(ctx context.Context) error {
	// This command is explicit and runs only once the replacement is healthy.
	out, err := exec.CommandContext(ctx, "docker", s.composeArgs("exec", "-T", s.mainServiceName, "wget", "-qO-", "http://127.0.0.1:8080/api/components")...).Output()
	if err != nil {
		return fmt.Errorf("check active Energyplan: %w", err)
	}
	var status struct {
		Optimizer struct {
			Bundled bool `json:"bundled_with_core"`
			Healthy bool `json:"healthy"`
		} `json:"optimizer"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return err
	}
	if !status.Optimizer.Bundled || !status.Optimizer.Healthy {
		return fmt.Errorf("install and select a healthy bundled Energyplan before retiring Python")
	}
	type change struct {
		path          string
		before, after []byte
		mode          os.FileMode
	}
	var changes []change
	for _, path := range s.composeFiles() {
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular Compose file %s", path)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		after, changed, err := retiredPythonCompose(before)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if changed {
			changes = append(changes, change{path, before, after, st.Mode().Perm()})
		}
	}
	ids, err := s.retiredPythonContainers(ctx)
	if err != nil {
		return err
	}
	suffix := ".before-python-removal-" + time.Now().UTC().Format("20060102T150405.000000000")
	for _, c := range changes {
		if err := os.WriteFile(c.path+suffix, c.before, c.mode); err != nil {
			return err
		}
	}
	restore := func(cause error) error {
		for _, c := range changes {
			if err := replaceRetiredCompose(c.path, c.before); err != nil {
				cause = errors.Join(cause, fmt.Errorf("restore %s: %w", c.path, err))
			}
		}
		return cause
	}
	for _, c := range changes {
		if err := replaceRetiredCompose(c.path, c.after); err != nil {
			return restore(err)
		}
	}
	if err := s.runner(ctx, nil, s.composeArgs("config", "--quiet")...); err != nil {
		return restore(fmt.Errorf("Compose validation failed: %w", err))
	}
	for _, id := range ids {
		if err := s.runner(ctx, nil, "rm", "--force", id); err != nil {
			return restore(fmt.Errorf("remove retired container: %w", err))
		}
	}
	fmt.Println("Python optimizer removed. Compose backups:", suffix, "Recreate Core at its pinned version to release the old IPC mount.")
	return nil
}

// Stage and fsync the new file before replacing it; retain the operator's owner and mode.
func replaceRetiredCompose(path string, data []byte) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular Compose file: %s", path)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ftw-retire-python-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if stat, ok := st.Sys().(*syscall.Stat_t); ok {
		if err = f.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
	}
	if err = f.Chmod(st.Mode().Perm()); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
