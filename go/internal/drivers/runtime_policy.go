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
	// ConfigSecrets comes from verified signed metadata. A read-only OAuth
	// driver may persist only these keys in its own secret namespace.
	ConfigSecrets []string
}

func (p *RuntimePolicy) allowsSecretPersistence(key string) bool {
	if p == nil {
		return true
	}
	if !p.IsReadOnly() || p.AuthPostPath == "" || !p.Permissions["http.get"] {
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
			if p.AuthPostPath == "" || !p.Permissions["http.get"] {
				return errors.New("read-only HTTP POST requires a declared auth path and http.get")
			}
		default:
			return fmt.Errorf("read-only runtime has write-capable permission %q", permission)
		}
	}
	return nil
}

func (p *RuntimePolicy) allows(permission string) bool {
	return p == nil || p.Permissions[permission]
}
