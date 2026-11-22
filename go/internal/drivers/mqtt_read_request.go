package drivers

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A read request asks a device to report and changes nothing on it. Venus OS
// on a Victron GX publishes its MQTT values only while some client sends one
// to R/<portal ID>/keepalive at least once a minute (victronenergy/dbus-flashmq
// README, "Keep-alive"). Without it the GX goes quiet and the driver goes
// stale, so a read-only driver that may subscribe may send the read requests
// its signed policy names. A read request needs no write permission, so a
// signed policy carries no mqtt.publish for it, and a Core that does not know
// this field runs the same driver read-only, refusing the request. The host
// holds each request to these bounds:
//
//   - topic: a declared pattern, matched level by level, where "+" stands for
//     one whole non-empty level and "#" is refused. The first level must be
//     exactly "R", Venus's read-request namespace. A literal first level alone
//     would not be enough: W/+/settings/0/Settings/CGwacs/AcPowerSetPoint is a
//     valid pattern and a Venus write, and a third-party repository's key,
//     not ours, may have signed the manifest. Topics are printable ASCII
//     without spaces, at most 256 bytes.
//   - payload: at most 1 KiB. A Venus keep-alive is empty or a JSON option
//     list of about 45 bytes.
//   - rate: six per driver in any 60 s. Venus needs one a minute and its own
//     example sends one every 30 s.
//
// That caps a declared topic at about 130 bytes a second, too little to carry
// anything but the request.
//
// A read request still waits out setup measurement mode and is never sent
// from a fingerprint probe, as every write does; see allowMQTTPublish.
const (
	// mqttReadRequestNamespace is the first level every read-request topic
	// has: Venus OS reads R/<portal ID>/... as a request to report.
	mqttReadRequestNamespace       = "R"
	maxMQTTReadRequestTopicBytes   = 256
	maxMQTTReadRequestPayloadBytes = 1024
	mqttReadRequestsPerWindow      = 6
	mqttReadRequestWindow          = time.Minute
)

// validMQTTReadRequestPattern reports why pattern cannot be a declared read
// request, or nil when it can.
func validMQTTReadRequestPattern(pattern string) error {
	if pattern == "" || len(pattern) > maxMQTTReadRequestTopicBytes {
		return fmt.Errorf("MQTT read-request topic %q must be 1 to %d bytes", pattern, maxMQTTReadRequestTopicBytes)
	}
	levels := strings.Split(pattern, "/")
	if levels[0] != mqttReadRequestNamespace || len(levels) < 2 {
		return fmt.Errorf("MQTT read-request topic %q must start with %s/, Venus's read-request namespace", pattern, mqttReadRequestNamespace)
	}
	for _, level := range levels[1:] {
		if level != "+" && !validMQTTTopicLevel(level) {
			return fmt.Errorf("MQTT read-request topic %q has level %q; a level is printable ASCII without spaces, and a wildcard is a whole \"+\" level", pattern, level)
		}
	}
	return nil
}

// validMQTTTopicLevel reports whether level is non-empty printable ASCII with
// no space and no wildcard character.
func validMQTTTopicLevel(level string) bool {
	if level == "" {
		return false
	}
	for i := 0; i < len(level); i++ {
		if c := level[i]; c <= ' ' || c > '~' || c == '+' || c == '#' {
			return false
		}
	}
	return true
}

// matchMQTTReadRequest reports whether topic is one the pattern declares. An
// invalid pattern matches nothing.
func matchMQTTReadRequest(pattern, topic string) bool {
	if validMQTTReadRequestPattern(pattern) != nil || len(topic) > maxMQTTReadRequestTopicBytes {
		return false
	}
	want := strings.Split(pattern, "/")
	got := strings.Split(topic, "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if !validMQTTTopicLevel(got[i]) || want[i] != "+" && want[i] != got[i] {
			return false
		}
	}
	return true
}

// allowsMQTTReadRequest reports whether this read-only policy grants a read
// request to topic: it may subscribe and declares a pattern topic matches. It
// checks each pattern as validate does, so a policy that reached a host
// without validation grants nothing more.
func (p *RuntimePolicy) allowsMQTTReadRequest(topic string) bool {
	if !p.IsReadOnly() || !p.Permissions["mqtt.subscribe"] {
		return false
	}
	for _, pattern := range p.MQTTReadRequestTopics {
		if matchMQTTReadRequest(pattern, topic) {
			return true
		}
	}
	return false
}

// allowMQTTPublish gates host.mqtt_publish. A publish from a signed read-only
// driver to a topic its policy declares is a read request, not a write, so it
// passes although the driver may not write; the payload and rate bounds above
// hold it instead. Every other publish, and every publish from a driver
// without a policy, goes through allowWrite unchanged.
//
// A read request still meets allowWrite's other gates. Setup measurement mode
// holds it, so a GX with no other MQTT client goes quiet until setup ends, and
// a fingerprint probe never sends one. DeviceReadOnly is left out: it means
// another client owns the driver's Modbus endpoint, and a read request goes to
// the MQTT broker, not to that endpoint.
func (h *HostEnv) allowMQTTPublish(topic string, payloadBytes int) error {
	if !h.RuntimePolicy.allowsMQTTReadRequest(topic) {
		return h.allowWrite("mqtt.publish")
	}
	if h.ControlBlocked != nil && h.ControlBlocked() {
		return errors.New("mqtt.publish: setup measurement mode blocks device writes")
	}
	if h.ProbeReadOnly {
		return errors.New("mqtt.publish: fingerprint probe cannot write")
	}
	if payloadBytes > maxMQTTReadRequestPayloadBytes {
		return fmt.Errorf("mqtt.publish: read request payload is %d bytes; the limit is %d bytes",
			payloadBytes, maxMQTTReadRequestPayloadBytes)
	}
	return h.readRequests.take()
}

// mqttReadRequestLimiter counts the read requests one driver sent in the last
// window. A refused request does not count.
type mqttReadRequestLimiter struct {
	mu   sync.Mutex
	sent []time.Time
	now  func() time.Time // nil means time.Now; tests set it
}

func (l *mqttReadRequestLimiter) take() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	kept := l.sent[:0]
	for _, at := range l.sent {
		if now.Sub(at) < mqttReadRequestWindow {
			kept = append(kept, at)
		}
	}
	l.sent = kept
	if len(l.sent) >= mqttReadRequestsPerWindow {
		return fmt.Errorf("mqtt.publish: read requests are limited to %d per minute", mqttReadRequestsPerWindow)
	}
	l.sent = append(l.sent, now)
	return nil
}
