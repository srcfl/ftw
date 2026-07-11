package nova

import "testing"

func TestDerName_Shape(t *testing.T) {
	got := DerName("ferroamp", KindBattery)
	if got != "ferroamp-battery" {
		t.Fatalf("DerName: got %s", got)
	}
}

func TestTopicFor_SanitizesDangerousChars(t *testing.T) {
	// ep:192.168.1.10:502 would create extra NATS subject levels via
	// Nova's MQTT adapter because '.' is the subject separator.
	// Same risk with ':', '/', '+', '#'.
	topic := TopicFor("f42w-1", "ep:192.168.1.10:502", "meter-0")
	want := "gateways/f42w-1/devices/ep_192_168_1_10_502/ders/meter-0/telemetry/json/v1"
	if topic != want {
		t.Fatalf("topic: got %s\nwant %s", topic, want)
	}
}

func TestTopicFor_PreservesDashAndUnderscore(t *testing.T) {
	topic := TopicFor("f42w-gw-1", "ferroamp_ES9234", "ferroamp-battery")
	want := "gateways/f42w-gw-1/devices/ferroamp_ES9234/ders/ferroamp-battery/telemetry/json/v1"
	if topic != want {
		t.Fatalf("topic: got %s", topic)
	}
}

func TestDeviceTypeFor(t *testing.T) {
	cases := []struct {
		kinds []string
		want  string
	}{
		{[]string{KindBattery, KindPV, KindMeter}, "inverter"}, // hybrid
		{[]string{KindPV}, "inverter"},                         // string inverter
		{[]string{KindMeter}, "meter"},
		{[]string{KindEV}, "charger"},
		{[]string{KindV2X}, "charger"},
		{[]string{}, "inverter"}, // default when unknown
	}
	for _, c := range cases {
		if got := DeviceTypeFor(c.kinds); got != c.want {
			t.Fatalf("%v → got %s, want %s", c.kinds, got, c.want)
		}
	}
}
