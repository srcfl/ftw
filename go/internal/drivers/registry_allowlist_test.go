package drivers

import (
	"reflect"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestMergeAllowedHosts(t *testing.T) {
	cases := []struct {
		name     string
		explicit []string
		cfg      map[string]any
		want     []string
	}{
		{
			name:     "explicit only",
			explicit: []string{"10.0.0.1"},
			cfg:      nil,
			want:     []string{"10.0.0.1"},
		},
		{
			name:     "config.host folded in",
			explicit: nil,
			cfg:      map[string]any{"host": "192.168.1.248"},
			want:     []string{"192.168.1.248"},
		},
		{
			name:     "config.host already listed — no duplicate",
			explicit: []string{"192.168.1.248"},
			cfg:      map[string]any{"host": "192.168.1.248"},
			want:     []string{"192.168.1.248"},
		},
		{
			name:     "config.host adds to explicit list",
			explicit: []string{"10.0.0.1"},
			cfg:      map[string]any{"host": "192.168.1.248"},
			want:     []string{"10.0.0.1", "192.168.1.248"},
		},
		{
			name:     "config.url host extracted",
			explicit: nil,
			cfg:      map[string]any{"url": "http://meter.local:8080/api"},
			want:     []string{"meter.local:8080"},
		},
		{
			name:     "config.host with whitespace trimmed",
			explicit: nil,
			cfg:      map[string]any{"host": "  192.168.1.248  "},
			want:     []string{"192.168.1.248"},
		},
		{
			name:     "non-string host ignored",
			explicit: []string{"10.0.0.1"},
			cfg:      map[string]any{"host": 12345},
			want:     []string{"10.0.0.1"},
		},
		{
			name:     "empty host ignored",
			explicit: nil,
			cfg:      map[string]any{"host": ""},
			want:     []string{},
		},
		{
			name:     "nil cfg returns explicit unchanged",
			explicit: []string{"a", "b"},
			cfg:      nil,
			want:     []string{"a", "b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeAllowedHosts(tc.explicit, tc.cfg)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mergeAllowedHosts() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TCP gets a tighter default than HTTP/WS: when the driver config supplies
// both host and port, the auto-injected allowlist entry is `host:port`
// (not bare host). Raw TCP can reach any service on the same IP, so
// "P1 reader on :23" must not also grant access to SSH on :22.
func TestTcpAllowedHostsFor(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Driver
		want []string
	}{
		{
			name: "config.host+port → tight host:port entry",
			cfg: config.Driver{
				Capabilities: config.Capabilities{TCP: &config.TCPCapability{}},
				Config:       map[string]any{"host": "192.168.1.40", "port": 23},
			},
			want: []string{"192.168.1.40:23"},
		},
		{
			name: "config.host without port falls back to bare host",
			cfg: config.Driver{
				Capabilities: config.Capabilities{TCP: &config.TCPCapability{}},
				Config:       map[string]any{"host": "192.168.1.40"},
			},
			want: []string{"192.168.1.40"},
		},
		{
			name: "explicit allowlist preserved, config.host:port appended",
			cfg: config.Driver{
				Capabilities: config.Capabilities{TCP: &config.TCPCapability{
					AllowedHosts: []string{"10.0.0.5", "loose-host"},
				}},
				Config: map[string]any{"host": "192.168.1.40", "port": 23},
			},
			want: []string{"10.0.0.5", "loose-host", "192.168.1.40:23"},
		},
		{
			name: "duplicate entry dropped",
			cfg: config.Driver{
				Capabilities: config.Capabilities{TCP: &config.TCPCapability{
					AllowedHosts: []string{"192.168.1.40:23"},
				}},
				Config: map[string]any{"host": "192.168.1.40", "port": 23},
			},
			want: []string{"192.168.1.40:23"},
		},
		{
			name: "yaml-decoded port (int) is honoured",
			cfg: config.Driver{
				Capabilities: config.Capabilities{TCP: &config.TCPCapability{}},
				Config:       map[string]any{"host": "192.168.1.40", "port": int64(2300)},
			},
			want: []string{"192.168.1.40:2300"},
		},
		{
			name: "no config at all returns empty (no implicit any-host)",
			cfg: config.Driver{
				Capabilities: config.Capabilities{TCP: &config.TCPCapability{}},
			},
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tcpAllowedHostsFor(tc.cfg)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tcpAllowedHostsFor() = %v, want %v", got, tc.want)
			}
		})
	}
}
