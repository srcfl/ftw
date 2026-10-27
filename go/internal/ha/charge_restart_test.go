package ha

import (
	"fmt"
	"testing"

	paho "github.com/eclipse/paho.mqtt.golang"
)

type retainedModeMessage struct {
	commandMessage
	retained bool
}

func (m retainedModeMessage) Retained() bool { return m.retained }

func TestRetainedChargeCannotRestartManualCharging(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		retained bool
		want     bool
	}{
		{"charge", true, false},
		{"charge", false, true},
		{"idle", true, true},
		{"planner_passive_arbitrage", true, true},
	} {
		t.Run(fmt.Sprintf("%s/retained=%t", tc.mode, tc.retained), func(t *testing.T) {
			cli := &commandClient{handlers: map[string]paho.MessageHandler{}}
			var called bool
			b := &Bridge{topicPrefix: "ftw", client: cli, cb: CommandCallbacks{SetMode: func(mode string) error {
				called = true
				if mode != tc.mode {
					t.Errorf("mode = %q, want %q", mode, tc.mode)
				}
				return nil
			}}}
			b.subscribeCommands()
			cli.handlers[b.cmdTopic("mode")](cli, retainedModeMessage{commandMessage: commandMessage{payload: []byte(tc.mode)}, retained: tc.retained})
			if called != tc.want {
				t.Fatalf("SetMode called = %v, want %v", called, tc.want)
			}
		})
	}
}
