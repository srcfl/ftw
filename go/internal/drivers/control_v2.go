package drivers

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	ControlRuntimeABIV2       = "gopher-lua-source-v2"
	ControlHostAPIProfileV2   = "sourceful.host/ftw-core/v2"
	DriverCommandSchemaV1     = "sourceful.driver-command/v1"
	DriverCommandResultSchema = "sourceful.driver-command-result/v1"
	defaultMaxWritesPerCall   = 128
)

var controlTokenRE = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
var controlHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RuntimePolicy is the verified, signed package policy bound to one managed
// artifact. SiteEnabled becomes true only when the local config pins the same
// package ID, version and artifact hash.
type RuntimePolicy struct {
	PackageID      string
	Version        string
	ArtifactSHA256 string
	RuntimeABI     string
	HostAPIProfile string
	Permissions    map[string]bool
	Commands       map[string]RuntimeCommand
	DefaultMode    string
	Lease          RuntimeLeasePolicy
	SiteEnabled    bool
	MaxWrites      int
}

type RuntimeCommand struct {
	ID            string
	RuntimeAction string
	Inputs        map[string]RuntimeCommandInput
}

type RuntimeCommandInput struct {
	Type     string
	Required bool
}

type RuntimeLeasePolicy struct {
	MaxDuration       time.Duration
	HeartbeatInterval time.Duration
	ExpiryAction      string
}

func (p *RuntimePolicy) IsControlV2() bool {
	return p != nil && p.RuntimeABI == ControlRuntimeABIV2 && p.HostAPIProfile == ControlHostAPIProfileV2
}

func (p *RuntimePolicy) validate() error {
	if !p.IsControlV2() || !strings.HasPrefix(p.PackageID, "com.sourceful.driver.") || p.Version == "" ||
		!controlHashRE.MatchString(p.ArtifactSHA256) || p.DefaultMode != "driver_default_mode_v2" {
		return errors.New("invalid control v2 runtime identity")
	}
	if p.Lease.MaxDuration < time.Second || p.Lease.MaxDuration > 300*time.Second ||
		p.Lease.HeartbeatInterval < time.Second || p.Lease.HeartbeatInterval > p.Lease.MaxDuration ||
		p.Lease.ExpiryAction != "return_to_default" {
		return errors.New("invalid control v2 lease policy")
	}
	hasWrite := false
	for permission, allowed := range p.Permissions {
		if !allowed {
			continue
		}
		switch permission {
		case "http.get", "http.post", "modbus.read", "modbus.write", "mqtt.publish", "mqtt.subscribe":
		default:
			return fmt.Errorf("unsupported control v2 permission %q", permission)
		}
		if permission == "http.post" || permission == "modbus.write" || permission == "mqtt.publish" {
			hasWrite = true
		}
	}
	if !hasWrite || (p.Permissions["modbus.write"] && !p.Permissions["modbus.read"]) {
		return errors.New("control v2 policy lacks a verifiable write path")
	}
	if len(p.Commands) == 0 || len(p.Commands) > 128 {
		return errors.New("control v2 policy has an invalid command count")
	}
	actions := make(map[string]bool, len(p.Commands))
	for id, command := range p.Commands {
		if id != command.ID || !validControlToken(id) || !validControlToken(command.RuntimeAction) || actions[command.RuntimeAction] {
			return errors.New("control v2 policy has an invalid or repeated command")
		}
		actions[command.RuntimeAction] = true
		if len(command.Inputs) > 32 {
			return fmt.Errorf("control v2 command %q has too many inputs", id)
		}
		for name, input := range command.Inputs {
			if !validControlToken(name) || (input.Type != "number" && input.Type != "boolean" && input.Type != "string") {
				return fmt.Errorf("control v2 command %q has an invalid input", id)
			}
		}
	}
	return nil
}

func (p *RuntimePolicy) allows(permission string) bool {
	return p == nil || p.Permissions[permission]
}

func (p *RuntimePolicy) maxWrites() int {
	if p == nil || p.MaxWrites <= 0 || p.MaxWrites > defaultMaxWritesPerCall {
		return defaultMaxWritesPerCall
	}
	return p.MaxWrites
}

func (p *RuntimePolicy) requiredEvidence() string {
	if p != nil && p.Permissions["modbus.write"] {
		return "readback"
	}
	return "write_ack"
}

// DriverCommandV1 is the host-owned command passed to driver_command_v2.
type DriverCommandV1 struct {
	SchemaVersion string                 `json:"schema_version"`
	ID            string                 `json:"id"`
	Command       string                 `json:"command"`
	Source        string                 `json:"source"`
	IssuedAt      time.Time              `json:"issued_at"`
	ExpiresAt     time.Time              `json:"expires_at"`
	Attempt       int                    `json:"attempt"`
	Lease         DriverCommandLeaseV1   `json:"lease"`
	Inputs        map[string]interface{} `json:"inputs"`
}

type DriverCommandLeaseV1 struct {
	ID                  string    `json:"id"`
	ExpiresAt           time.Time `json:"expires_at"`
	HeartbeatIntervalMS int64     `json:"heartbeat_interval_ms"`
}

// DriverCommandResultV1 is completed by the host after Lua returns.
type DriverCommandResultV1 struct {
	SchemaVersion string                 `json:"schema_version"`
	ID            string                 `json:"id"`
	Command       string                 `json:"command"`
	LeaseID       string                 `json:"lease_id,omitempty"`
	Status        string                 `json:"status"`
	Code          string                 `json:"code"`
	Message       string                 `json:"message,omitempty"`
	CompletedAt   time.Time              `json:"completed_at"`
	DeviceState   string                 `json:"device_state"`
	Writes        int                    `json:"writes"`
	Evidence      []string               `json:"evidence,omitempty"`
	Applied       map[string]interface{} `json:"applied,omitempty"`
	Driver        DriverResultIdentityV1 `json:"driver"`
}

type DriverResultIdentityV1 struct {
	PackageID      string `json:"package_id"`
	Version        string `json:"version"`
	ArtifactSHA256 string `json:"artifact_sha256"`
}

type luaCommandResultV2 struct {
	Status      string
	Code        string
	Message     string
	DeviceState string
	Evidence    []string
	Applied     map[string]interface{}
}

func (p *RuntimePolicy) validateCommand(cmd DriverCommandV1, now time.Time) (RuntimeCommand, error) {
	if !p.IsControlV2() {
		return RuntimeCommand{}, errors.New("driver control v2 policy is not active")
	}
	if !p.SiteEnabled {
		return RuntimeCommand{}, errors.New("driver control is not enabled for this site and exact artifact")
	}
	if cmd.SchemaVersion != DriverCommandSchemaV1 || !validControlID(cmd.ID) ||
		!validControlToken(cmd.Command) || !validControlToken(cmd.Source) || cmd.Attempt < 1 || cmd.Attempt > 16 {
		return RuntimeCommand{}, errors.New("invalid sourceful.driver-command/v1 envelope")
	}
	if cmd.IssuedAt.IsZero() || cmd.ExpiresAt.IsZero() || !cmd.ExpiresAt.After(cmd.IssuedAt) || !now.Before(cmd.ExpiresAt) {
		return RuntimeCommand{}, errors.New("driver command has expired or has invalid times")
	}
	if cmd.IssuedAt.After(now.Add(30*time.Second)) || cmd.ExpiresAt.After(now.Add(30*time.Second)) {
		return RuntimeCommand{}, errors.New("driver command time window exceeds the host limit")
	}
	heartbeat := time.Duration(cmd.Lease.HeartbeatIntervalMS) * time.Millisecond
	if !validControlID(cmd.Lease.ID) || cmd.Lease.ExpiresAt.IsZero() || !now.Before(cmd.Lease.ExpiresAt) ||
		heartbeat < 100*time.Millisecond || heartbeat > 300*time.Second || heartbeat != p.Lease.HeartbeatInterval {
		return RuntimeCommand{}, errors.New("driver command has an invalid lease")
	}
	if p.Lease.MaxDuration <= 0 || cmd.Lease.ExpiresAt.After(now.Add(p.Lease.MaxDuration)) ||
		!cmd.Lease.ExpiresAt.After(now.Add(heartbeat)) {
		return RuntimeCommand{}, errors.New("driver command lease exceeds signed package limit")
	}
	decl, ok := p.Commands[cmd.Command]
	if !ok {
		return RuntimeCommand{}, fmt.Errorf("driver command %q is not declared by the signed package", cmd.Command)
	}
	if len(cmd.Inputs) > 32 {
		return RuntimeCommand{}, errors.New("driver command has too many inputs")
	}
	for name, value := range cmd.Inputs {
		input, ok := decl.Inputs[name]
		if !ok {
			return RuntimeCommand{}, fmt.Errorf("driver command input %q is not declared", name)
		}
		if err := validateCommandInput(name, value, input.Type); err != nil {
			return RuntimeCommand{}, err
		}
	}
	for name, input := range decl.Inputs {
		if _, ok := cmd.Inputs[name]; input.Required && !ok {
			return RuntimeCommand{}, fmt.Errorf("driver command input %q is required", name)
		}
	}
	return decl, nil
}

func validateCommandInput(name string, value interface{}, typ string) error {
	valid := false
	switch typ {
	case "number":
		_, valid = value.(float64)
	case "boolean":
		_, valid = value.(bool)
	case "string":
		v, ok := value.(string)
		valid = ok && len(v) <= 512
	}
	if !valid {
		return fmt.Errorf("driver command input %q must be %s", name, typ)
	}
	return nil
}

func parseLuaCommandResult(value interface{}) (luaCommandResultV2, error) {
	m, ok := value.(map[string]interface{})
	if !ok {
		return luaCommandResultV2{}, errors.New("driver_command_v2 must return a table")
	}
	allowed := map[string]bool{
		"status": true, "code": true, "message": true, "device_state": true,
		"evidence": true, "applied": true,
	}
	for key := range m {
		if !allowed[key] {
			return luaCommandResultV2{}, fmt.Errorf("driver result has undeclared field %q", key)
		}
	}
	result := luaCommandResultV2{}
	result.Status, _ = m["status"].(string)
	result.Code, _ = m["code"].(string)
	result.Message, _ = m["message"].(string)
	result.DeviceState, _ = m["device_state"].(string)
	if !map[string]bool{"accepted": true, "applied": true, "rejected": true, "failed": true, "expired": true, "defaulted": true}[result.Status] {
		return luaCommandResultV2{}, fmt.Errorf("driver result status %q is invalid", result.Status)
	}
	if !validControlToken(result.Code) {
		return luaCommandResultV2{}, fmt.Errorf("driver result code %q is invalid", result.Code)
	}
	if len(result.Message) > 512 {
		return luaCommandResultV2{}, errors.New("driver result message exceeds 512 bytes")
	}
	if !map[string]bool{"controlled": true, "default": true, "unchanged": true, "unknown": true}[result.DeviceState] {
		return luaCommandResultV2{}, fmt.Errorf("driver result device_state %q is invalid", result.DeviceState)
	}
	if raw, ok := m["evidence"]; ok {
		list, ok := raw.([]interface{})
		if !ok || len(list) > 3 {
			return luaCommandResultV2{}, errors.New("driver result evidence must be an array of at most three values")
		}
		seen := map[string]bool{}
		for _, item := range list {
			name, ok := item.(string)
			if !ok || !map[string]bool{"write_ack": true, "vendor_ack": true, "readback": true}[name] || seen[name] {
				return luaCommandResultV2{}, errors.New("driver result evidence is invalid")
			}
			seen[name] = true
			result.Evidence = append(result.Evidence, name)
		}
	}
	if raw, ok := m["applied"]; ok {
		applied, ok := raw.(map[string]interface{})
		if !ok || len(applied) > 32 {
			return luaCommandResultV2{}, errors.New("driver result applied must be an object with at most 32 values")
		}
		for key, value := range applied {
			if !validControlToken(key) {
				return luaCommandResultV2{}, fmt.Errorf("driver result applied key %q is invalid", key)
			}
			switch v := value.(type) {
			case string:
				if len(v) > 512 {
					return luaCommandResultV2{}, fmt.Errorf("driver result applied value %q is too long", key)
				}
			case float64, bool:
			default:
				return luaCommandResultV2{}, fmt.Errorf("driver result applied value %q is not scalar", key)
			}
		}
		result.Applied = applied
	}
	return result, nil
}

func (r DriverCommandResultV1) JSON() []byte {
	b, _ := json.Marshal(r)
	return b
}

func validControlToken(value string) bool {
	return len(value) <= 128 && controlTokenRE.MatchString(value)
}

func validControlID(value string) bool {
	return len(value) >= 16 && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '.' && r != '_' && r != ':' && r != '-'
	}) == -1
}
