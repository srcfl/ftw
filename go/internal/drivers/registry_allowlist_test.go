package drivers

import (
	"reflect"
	"testing"
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
