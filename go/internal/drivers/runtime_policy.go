package drivers

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var controlHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
var persistSecretKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func validPersistSecretKey(key string) bool { return persistSecretKeyRE.MatchString(key) }

// RuntimePolicy is the verified, signed policy bound to one managed read-only
// artifact. A signed driver that may control runs without a policy, under the
// same terms as its bundled copy, so validate accepts only read-only policies.
type RuntimePolicy struct {
	PackageID      string
	Version        string
	ArtifactSHA256 string
	RuntimeABI     string
	HostAPIProfile string
	ReadOnly       bool
	Permissions    map[string]bool
	// AuthPostPath is the one URL path a read-only driver may POST to. Some
	// read-only drivers read a vendor cloud and cannot read anything until
	// they have exchanged a token, and a token exchange is a POST issued from
	// init or poll, which allowWrite refuses. Empty for every driver that
	// does not declare one, which is all of them by default.
	AuthPostPath string
	// AuthPostPaths adds further exact sign-in paths, for a login that posts
	// more than one form (an OIDC identifier step, then a password step).
	AuthPostPaths []string
	// MQTTReadRequestTopics are the MQTT topics a read-only driver with
	// mqtt.subscribe may publish read requests to, because the device
	// publishes its values only while a client asks (Victron's Venus OS wants
	// R/<portal ID>/keepalive). They grant no write permission: mqtt.publish
	// stays refused. Each is a pattern whose "+" stands for one whole topic
	// level; see mqtt_read_request.go. Empty unless the signed metadata
	// declares them.
	MQTTReadRequestTopics []string
	// ConfigSecrets comes from verified signed metadata. A read-only OAuth
	// driver may persist only these keys in its own secret namespace.
	ConfigSecrets []string
	// Capabilities are the signed catalog entry's, not what the Lua file
	// says of itself. host.site_location() reads them.
	Capabilities []string
}

// Lists reports whether the signed catalog entry lists capability.
func (p *RuntimePolicy) Lists(capability string) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Capabilities {
		if strings.EqualFold(c, capability) {
			return true
		}
	}
	return false
}

// authPaths returns every declared sign-in path.
func (p *RuntimePolicy) authPaths() []string {
	if p == nil {
		return nil
	}
	var paths []string
	if p.AuthPostPath != "" {
		paths = append(paths, p.AuthPostPath)
	}
	for _, path := range p.AuthPostPaths {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func (p *RuntimePolicy) allowsSecretPersistence(key string) bool {
	if p == nil {
		return true
	}
	if !p.IsReadOnly() || len(p.authPaths()) == 0 || !p.Permissions["http.get"] {
		return false
	}
	for _, allowed := range p.ConfigSecrets {
		if key == allowed {
			return true
		}
	}
	return false
}

func (p *RuntimePolicy) IsReadOnly() bool {
	return p != nil && p.ReadOnly
}

func (p *RuntimePolicy) validate() error {
	if !p.IsReadOnly() {
		return errors.New("managed driver runtime policy must be read-only")
	}
	if !strings.HasPrefix(p.PackageID, "com.sourceful.driver.") || p.Version == "" ||
		!controlHashRE.MatchString(p.ArtifactSHA256) || p.RuntimeABI == "" || p.HostAPIProfile == "" {
		return errors.New("invalid read-only runtime identity")
	}
	for permission, allowed := range p.Permissions {
		if !allowed {
			continue
		}
		switch permission {
		case "http.get", "modbus.read", "mqtt.subscribe", "serial.read":
		case "http.post":
			if len(p.authPaths()) == 0 || !p.Permissions["http.get"] {
				return errors.New("read-only HTTP POST requires a declared auth path and http.get")
			}
		default:
			return fmt.Errorf("read-only runtime has write-capable permission %q", permission)
		}
	}
	// Declared read requests need no write permission; mqtt.publish stays
	// refused above. Refuse a bad pattern here, so a malformed signed entry
	// fails at start.
	for _, pattern := range p.MQTTReadRequestTopics {
		if err := validMQTTReadRequestPattern(pattern); err != nil {
			return err
		}
	}
	return nil
}

func (p *RuntimePolicy) allows(permission string) bool {
	return p == nil || p.Permissions[permission]
}
