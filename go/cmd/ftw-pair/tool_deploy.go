package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// Compile-time check: DeployDriverTool must satisfy the Tool interface.
var _ Tool = (*DeployDriverTool)(nil)

// DeployDriverTool is a multi-step wrapper that:
//  1. Writes the Lua driver source to drivers/<name>.lua within the repo.
//  2. Patches the config.yaml drivers list (upsert by name).
//  3. Polls GET <apiBase>/api/drivers/<name> until tick_count > 0 (up to 10 s).
type DeployDriverTool struct {
	scope          *Scope
	audit          *Audit
	apiBase        string
	configPath     string
	userDriversDir string // persistent overlay dir; written here when set
	client         *http.Client
}

// NewDeployDriverTool constructs a DeployDriverTool. apiBase is the base URL of
// the running FTW service (e.g. "http://localhost:8080").
// userDriversDir is the persistent user-drivers directory (e.g. /app/data/drivers
// in the docker deploy). When non-empty, Lua files are written there so they
// survive docker image updates. When empty, the old behaviour (sibling of configPath)
// is preserved.
func NewDeployDriverTool(sc *Scope, a *Audit, apiBase, configPath, userDriversDir string) *DeployDriverTool {
	return &DeployDriverTool{
		scope:          sc,
		audit:          a,
		apiBase:        strings.TrimRight(apiBase, "/"),
		configPath:     configPath,
		userDriversDir: userDriversDir,
		client:         &http.Client{Timeout: 5 * time.Second},
	}
}

func (t *DeployDriverTool) Name() string { return "deploy_driver" }

func (t *DeployDriverTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "deploy_driver",
		Description: "Deploy a Lua driver: write the source file, patch config.yaml, then poll until the service picks it up (tick_count > 0). Returns {status, driver, tick_count, detail}.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Driver name (used as filename base, e.g. \"goodwe_xs\" → drivers/goodwe_xs.lua)",
				},
				"lua_source": map[string]any{
					"type":        "string",
					"description": "Full Lua source code for the driver",
				},
				"config": map[string]any{
					"type":        "object",
					"description": "Driver config block to merge into the drivers list in config.yaml (e.g. {capabilities:[\"pv\"], lua:\"drivers/goodwe_xs.lua\"})",
				},
			},
			"required": []string{"name", "lua_source", "config"},
		},
	}
}

// Handle implements the multi-step deploy:
//  1. Validate inputs.
//  2. Write the Lua source file.
//  3. Patch config.yaml.
//  4. Poll the API until tick_count > 0.
func (t *DeployDriverTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	// 1. Validate inputs.
	name, ok := args["name"].(string)
	if !ok || name == "" {
		return nil, fmt.Errorf("deploy_driver: name (string) is required")
	}
	luaSrc, ok := args["lua_source"].(string)
	if !ok || luaSrc == "" {
		return nil, fmt.Errorf("deploy_driver: lua_source (string) is required")
	}
	cfgBlock, ok := args["config"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("deploy_driver: config (object) is required")
	}

	// 2. Write the Lua source file.
	// When a persistent user-drivers directory is configured (docker deploy),
	// write there so the file survives image updates. Fall back to the
	// config-sibling drivers/ for non-docker setups.
	driverDir := filepath.Join(filepath.Dir(t.configPath), "drivers")
	if t.userDriversDir != "" {
		driverDir = t.userDriversDir
	}
	luaPath := filepath.Join(driverDir, name+".lua")

	resolvedLua, err := t.scope.Resolve(luaPath)
	if err != nil {
		return nil, fmt.Errorf("deploy_driver: lua path out of scope: %w", err)
	}

	luaBefore := ""
	if existing, readErr := os.ReadFile(resolvedLua); readErr == nil {
		luaBefore = string(existing)
	}

	if err := os.MkdirAll(filepath.Dir(resolvedLua), 0o755); err != nil {
		return nil, fmt.Errorf("deploy_driver: mkdir %s: %w", filepath.Dir(resolvedLua), err)
	}
	if err := os.WriteFile(resolvedLua, []byte(luaSrc), 0o644); err != nil {
		return nil, fmt.Errorf("deploy_driver: write lua: %w", err)
	}
	t.audit.RecordFileWrite(resolvedLua, luaBefore, luaSrc)

	// 3. Patch config.yaml: upsert driver entry by name.
	resolvedCfg, err := t.scope.Resolve(t.configPath)
	if err != nil {
		return nil, fmt.Errorf("deploy_driver: config path out of scope: %w", err)
	}

	cfgBefore := ""
	rawCfg, readErr := os.ReadFile(resolvedCfg)
	if readErr == nil {
		cfgBefore = string(rawCfg)
	}

	var topLevel map[string]any
	if readErr == nil {
		if err := yaml.Unmarshal(rawCfg, &topLevel); err != nil {
			return nil, fmt.Errorf("deploy_driver: parse config.yaml: %w", err)
		}
	}
	if topLevel == nil {
		topLevel = map[string]any{}
	}

	// Normalise the drivers list.
	var drivers []any
	if d, exists := topLevel["drivers"]; exists && d != nil {
		if sl, ok := d.([]any); ok {
			drivers = sl
		}
	}

	// Set name on the config block and default lua path if absent.
	cfgBlock["name"] = name
	if _, hasLua := cfgBlock["lua"]; !hasLua {
		cfgBlock["lua"] = "drivers/" + name + ".lua"
	}

	// Upsert: find existing entry by name → replace, else append.
	replaced := false
	for i, entry := range drivers {
		if em, ok := entry.(map[string]any); ok {
			if em["name"] == name {
				drivers[i] = cfgBlock
				replaced = true
				break
			}
		}
		// yaml.v3 may also decode as map[string]interface{} with string keys via
		// map[interface{}]interface{} on older versions — handle that too.
		if em, ok := entry.(map[interface{}]interface{}); ok {
			if em["name"] == name {
				drivers[i] = cfgBlock
				replaced = true
				break
			}
		}
	}
	if !replaced {
		drivers = append(drivers, cfgBlock)
	}
	topLevel["drivers"] = drivers

	cfgOut, err := yaml.Marshal(topLevel)
	if err != nil {
		return nil, fmt.Errorf("deploy_driver: marshal config.yaml: %w", err)
	}
	if err := os.WriteFile(resolvedCfg, cfgOut, 0o644); err != nil {
		return nil, fmt.Errorf("deploy_driver: write config.yaml: %w", err)
	}
	t.audit.RecordFileWrite(resolvedCfg, cfgBefore, string(cfgOut))

	// 4. Poll the API until tick_count > 0 (up to 10 s).
	deadline := time.Now().Add(10 * time.Second)
	var lastDriver map[string]any
	for time.Now().Before(deadline) {
		status, pollErr := t.getDriverStatus(ctx, name)
		if pollErr == nil {
			lastDriver = status
			tc := tickCount(status)
			if tc > 0 {
				return map[string]any{
					"status":     "ok",
					"driver":     name,
					"tick_count": tc,
					"detail":     status,
				}, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	return map[string]any{
		"status":  "timeout_waiting_for_tick",
		"driver":  name,
		"detail":  lastDriver,
	}, nil
}

// getDriverStatus performs a GET to <apiBase>/api/drivers/<name> and returns
// the decoded JSON response body as a map.
func (t *DeployDriverTool) getDriverStatus(ctx context.Context, name string) (map[string]any, error) {
	url := t.apiBase + "/api/drivers/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse driver status: %w", err)
	}
	return out, nil
}

// tickCount extracts tick_count from a driver status map, handling both int
// and float64 (JSON numbers decode as float64).
func tickCount(m map[string]any) int64 {
	if m == nil {
		return 0
	}
	switch v := m["tick_count"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}
