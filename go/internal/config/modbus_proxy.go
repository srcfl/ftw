package config

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
)

// DefaultModbusProxyListen is the bind used when the site has exactly one
// Modbus TCP endpoint and the driver does not set proxy_listen.
const DefaultModbusProxyListen = ":1502"

// ModbusProxy exposes driver Modbus TCP sessions on the LAN so other
// integrations can share the socket FTW already holds. Off by default:
// Modbus TCP has no authentication, and writes bypass the control loop.
type ModbusProxy struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	Listen     string `yaml:"listen,omitempty" json:"listen,omitempty"`
	AllowWrite bool   `yaml:"allow_write,omitempty" json:"allow_write,omitempty"`
}

// ModbusProxyBind is one listener attached to a driver Modbus TCP endpoint.
type ModbusProxyBind struct {
	Listen               string
	Host                 string
	Port                 int
	AllowUnverifiedLocal bool
}

// On reports whether the proxy should bind. Nil-safe.
func (p *ModbusProxy) On() bool {
	return p != nil && p.Enabled
}

// ListenAddr is the default bind for a single-endpoint site.
func (p *ModbusProxy) ListenAddr() string {
	if p != nil && strings.TrimSpace(p.Listen) != "" {
		return strings.TrimSpace(p.Listen)
	}
	return DefaultModbusProxyListen
}

func (c *Config) validateModbusProxy() error {
	if c == nil || !c.ModbusProxy.On() {
		return nil
	}
	if _, err := NormalizeListenAddr(c.ModbusProxy.ListenAddr()); err != nil {
		return fmt.Errorf("modbus_proxy.listen: %w", err)
	}
	if _, err := c.ModbusProxyBinds(); err != nil {
		return err
	}
	return nil
}

// ModbusProxyBinds is the listen/backend map the runtime engine should serve.
func (c *Config) ModbusProxyBinds() ([]ModbusProxyBind, error) {
	if c == nil || !c.ModbusProxy.On() {
		return nil, nil
	}
	type ep struct {
		host, listen    string
		port            int
		allowUnverified bool
		drivers         []string
	}
	byKey := map[string]*ep{}
	order := []string{}
	for _, d := range c.Drivers {
		if d.Disabled {
			continue
		}
		mb := d.EffectiveModbus()
		if mb == nil || strings.TrimSpace(mb.Host) == "" {
			continue
		}
		port := mb.Port
		if port == 0 {
			port = 502
		}
		key := net.JoinHostPort(mb.Host, strconv.Itoa(port))
		e := byKey[key]
		if e == nil {
			e = &ep{host: mb.Host, port: port, allowUnverified: d.Capabilities.AllowUnverifiedLocal}
			byKey[key] = e
			order = append(order, key)
		}
		if d.Capabilities.AllowUnverifiedLocal {
			e.allowUnverified = true
		}
		e.drivers = append(e.drivers, d.Name)
		pl := strings.TrimSpace(mb.ProxyListen)
		if pl == "" {
			continue
		}
		norm, err := NormalizeListenAddr(pl)
		if err != nil {
			return nil, fmt.Errorf("driver %q: proxy_listen: %w", d.Name, err)
		}
		if e.listen != "" && e.listen != norm {
			return nil, fmt.Errorf("modbus_proxy: endpoint %s has conflicting proxy_listen (%s vs %s)", key, e.listen, norm)
		}
		e.listen = norm
	}

	if len(order) == 0 {
		return nil, nil
	}
	if len(order) > 1 {
		for _, key := range order {
			if byKey[key].listen == "" {
				return nil, fmt.Errorf("modbus_proxy: multiple Modbus endpoints; set capabilities.modbus.proxy_listen on each (missing for %s, drivers %s)", key, strings.Join(byKey[key].drivers, ", "))
			}
		}
	}
	defListen, err := NormalizeListenAddr(c.ModbusProxy.ListenAddr())
	if err != nil {
		return nil, fmt.Errorf("modbus_proxy.listen: %w", err)
	}

	used := map[string]string{}
	out := make([]ModbusProxyBind, 0, len(order))
	for _, key := range order {
		e := byKey[key]
		listen := e.listen
		if listen == "" {
			listen = defListen
		}
		if other := used[listen]; other != "" {
			return nil, fmt.Errorf("modbus_proxy: listen %s used by both %s and %s", listen, other, key)
		}
		used[listen] = key
		out = append(out, ModbusProxyBind{
			Listen:               listen,
			Host:                 e.host,
			Port:                 e.port,
			AllowUnverifiedLocal: e.allowUnverified,
		})
	}
	return out, nil
}

// NormalizeListenAddr accepts ":1502", "1502", "0.0.0.0:1502".
func NormalizeListenAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = DefaultModbusProxyListen
	}
	if !strings.Contains(s, ":") {
		s = ":" + s
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("invalid listen address %q", s)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("invalid listen port in %q", s)
	}
	return net.JoinHostPort(host, port), nil
}

func modbusProxyRestartReasons(oldCfg, newCfg *Config) []string {
	var reasons []string
	if !reflect.DeepEqual(oldCfg.ModbusProxy, newCfg.ModbusProxy) {
		reasons = append(reasons, "modbus_proxy — TCP listener binds at startup")
	}
	if oldCfg.ModbusProxy.On() || newCfg.ModbusProxy.On() {
		if !reflect.DeepEqual(modbusProxySignature(oldCfg), modbusProxySignature(newCfg)) {
			reasons = append(reasons, "modbus_proxy endpoints — driver Modbus host/port/listen feeds the proxy at startup")
		}
	}
	return reasons
}

type proxySig struct {
	Host, Listen    string
	Port            int
	AllowUnverified bool
}

func modbusProxySignature(c *Config) []proxySig {
	if c == nil {
		return nil
	}
	var out []proxySig
	for _, d := range c.Drivers {
		if d.Disabled {
			continue
		}
		mb := d.EffectiveModbus()
		if mb == nil {
			continue
		}
		out = append(out, proxySig{
			Host:            mb.Host,
			Port:            mb.Port,
			Listen:          mb.ProxyListen,
			AllowUnverified: d.Capabilities.AllowUnverifiedLocal,
		})
	}
	return out
}
