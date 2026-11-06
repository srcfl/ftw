package cloudwire

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/srcfl/ftw/go/protocol"
)

// Golden vectors: fixed keys and bodies, and the exact bytes they make.
//
// testdata/cloud-vectors.json is what srcfl/ftw-cloud and the phone replay. A
// change to any body, key order, id or frame shows up here as a diff to that
// file, which has to be regenerated on purpose:
//
//	go test ./cloudwire -run TestGoldenVectors -update
//
// ECDSA signatures are randomised, so the stored signature is checked by
// verifying it, not by signing again.

var update = flag.Bool("update", false, "rewrite testdata/cloud-vectors.json")

const vectorsPath = "testdata/cloud-vectors.json"

type vectorFile struct {
	BoxKey     vectorKey      `json:"boxKey"`
	ServiceKey vectorKey      `json:"serviceKey"`
	GrantKey   string         `json:"grantKey"`
	IDs        []vectorID     `json:"ids"`
	Messages   []vectorSigned `json:"messages"`
}

type vectorKey struct {
	D  string `json:"d"`
	PK string `json:"pk"`
	// Home is the box key's home id. The service signs over the box's home
	// too, so its own key has none.
	Home string `json:"home,omitempty"`
}

type vectorID struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

type vectorSigned struct {
	Name string `json:"name"`
	// Signer is "box", "service" or "" for unsigned.
	Signer string `json:"signer"`
	T      string `json:"t"`
	Body   string `json:"body"`
	// SignedInputSHA256 is SHA-256 of SignTag ‖ CBOR([home, t, b]).
	SignedInputSHA256 string `json:"signedInputSha256"`
	Sig               string `json:"sig"`
	Envelope          string `json:"envelope"`
	Frame             string `json:"frame"`
}

func i64(v int64) *int64 { return &v }

// The two cars the vectors carry, by their 128-bit ids.
const (
	vectorCarTesla = "5f0c1e2d3a4b5c6d7e8f90a1b2c3d4e5"
	vectorCarVolvo = "0123456789abcdef0123456789abcdef"
)

// vectorBodies are the bodies the vectors freeze, in order.
func vectorBodies(grantKey []byte, boxPK []byte) []struct {
	name, signer, t string
	body            any
} {
	boot := bytes.Repeat([]byte{0xb0}, BootBytes)
	wall := int64(1_791_820_800_000) // 2026-10-12 17:20:00 UTC
	h := func(seq uint64, uptime int64) Header {
		return Header{Seq: seq, Boot: boot, UptimeMs: uptime, WallMs: i64(wall + uptime - 60_000)}
	}
	meter := SourceID(grantKey, "p1meter")
	inverter := SourceID(grantKey, "sungrow")
	charger := SourceID(grantKey, "easee")
	garage := LoadpointID(grantKey, "garage")
	tesla := CarID(grantKey, vectorCarTesla)
	volvo := CarID(grantKey, vectorCarVolvo)

	return []struct {
		name, signer, t string
		body            any
	}{
		{"challenge", "", MsgChallenge, Challenge{V: Version, Nonce: bytes.Repeat([]byte{0x11}, NonceBytes)}},
		{"hello", "box", MsgHello, Hello{
			Header: h(1, 60_000), V: Version, PK: boxPK,
			Nonce: bytes.Repeat([]byte{0x11}, NonceBytes), EKM: bytes.Repeat([]byte{0x22}, EKMBytes),
			BoxNonce: bytes.Repeat([]byte{0x33}, NonceBytes), Build: "0.142.0", Clock: ClockNTP,
			Caps: []string{MsgLive, MsgLP, MsgPrices, MsgHB, MsgRoster}, RosterSeq: 4,
			GrantID: bytes.Repeat([]byte{0x44}, GrantIDBytes),
		}},
		{"hello_ok", "service", MsgHelloOK, HelloOK{
			BoxNonce: bytes.Repeat([]byte{0x33}, NonceBytes), EKM: bytes.Repeat([]byte{0x22}, EKMBytes),
			Have: map[string]string{}, RosterSeq: 3,
		}},
		{"roster", "box", MsgRoster, Roster{
			Header: h(2, 60_010), RosterSeq: 4, TTLS: 86_400, Noise: bytes.Repeat([]byte{0x77}, 32),
			Devices: []Device{
				{ID: "q5Lw0Zb1", Key: bytes.Repeat([]byte{0x55}, 32), Role: "owner",
					Scopes: []string{protocol.ScopeLiveRead, protocol.ScopeHistoryRead, protocol.ScopePlanRead,
						protocol.ScopeHealthRead, protocol.ScopeEventsRead}, Epoch: 1},
				{ID: "Zf09xQ2a", Key: bytes.Repeat([]byte{0x66}, 32), Role: "viewer",
					Scopes: []string{protocol.ScopeLiveRead}, Epoch: 2},
			},
			Grant: Grant{ID: bytes.Repeat([]byte{0x44}, GrantIDBytes),
				Scopes:   []string{protocol.ScopeLiveRead, protocol.ScopeEventsRead},
				Purposes: []string{protocol.CloudPurposeServe}, Controller: ControllerSourceful},
		}},
		{"live full", "box", MsgLive, Live{
			Header: h(3, 60_020),
			Fields: map[string]int64{"1": 0, "2": 1840, "3": -2310, "4": 950, "5": 612, "6": 1100, "10": 2100},
			FieldSources: map[string][]string{
				"2": {meter}, "3": {inverter}, "4": {inverter}, "5": {inverter},
				"6": {charger, inverter, meter}, "10": {charger},
			},
			Sources: map[string]Source{
				meter:    {Kind: "driver", State: protocol.SourceLive, LastOkMs: 59_100, StaleAfterMs: 10_000},
				inverter: {Kind: "driver", State: protocol.SourceLive, LastOkMs: 57_400, StaleAfterMs: 15_000},
				charger:  {Kind: "driver", State: protocol.SourceLagging, LastOkMs: 1_000, StaleAfterMs: 30_000},
			},
			SrcRev: 1,
			Ages:   map[string]int64{meter: 920, inverter: 2620, charger: 59_020},
		}},
		{"live delta", "box", MsgLive, Live{
			Header: h(4, 64_500),
			Fields: map[string]int64{"1": 0, "2": 1795, "3": -2290, "4": 950, "5": 612, "6": 1035, "10": 2100},
			SrcRev: 1,
			Ages:   map[string]int64{meter: 400, inverter: 2100},
		}},
		{"lp", "box", MsgLP, LP{
			Header: h(5, 64_510), ControlRev: 812,
			Loadpoints: []Loadpoint{{
				ID: garage, Src: charger, Plugged: true, SocPermille: i64(430), SocSource: "inferred",
				SocAgeS: i64(1800), TargetPermille: i64(800), ReadyByMs: i64(1_791_867_600_000),
				Recurring: true, PowerW: 2100, SessionWh: 3400, Session: "s1",
				CommandedW: i64(2070), CommandedReason: "plan", PlanPending: false,
				NextStartMs: i64(1_791_850_000_000), NextEndMs: i64(1_791_857_200_000), NextWh: i64(7400),
			}},
			Feedback: []Feedback{{
				Kind: "ev", Src: charger, Status: "following", Reason: "plan", Severity: "ok",
				Evidence: "measured", Fresh: true, ActualW: i64(2100), AgeMs: i64(900),
			}},
		}},
		{"prices", "box", MsgPrices, Prices{
			Header: h(6, 64_520), Zone: "SE3", Currency: "SEK",
			T0Ms: 1_791_820_800_000, SlotMs: 900_000,
			SpotMinor:   []*int64{i64(42), i64(40), nil, i64(-3)},
			TotalMinor:  []*int64{i64(118), i64(115), nil, i64(71)},
			VATPermille: 250,
		}},
		{"hb", "box", MsgHB, HB{
			Header: h(7, 69_020), Clock: ClockNTP, SrcRev: 1, LiveSeq: 4, LpSeq: 5,
			Ages: map[string]int64{meter: 1_400, inverter: 3_100},
		}},
		{"watch", "", MsgWatch, Watch{Hz: 1}},
		{"resync", "", MsgResync, Resync{What: []string{ResyncFull}}},
		{"bye", "box", MsgBye, Bye{Header: h(8, 70_000), Why: ByeRevoked, Purge: true}},
		{"purged", "service", MsgPurged, Purged{AtMs: 1_791_821_000_000, What: PurgedHome,
			GrantID: bytes.Repeat([]byte{0x44}, GrantIDBytes)}},
		{"event connected", "box", MsgEvent, Event{
			Header: h(9, 71_000), EventID: "Q2ZrMjR0dzVh", Ts: i64(wall + 71_000 - 60_000 - 400),
			Kind: protocol.PushChargingConnected, Severity: SeverityInfo, Args: map[string]string{},
			Src: charger, LP: garage,
		}},
		{"event interrupted", "box", MsgEvent, Event{
			Header: h(10, 72_000), EventID: "aW50cnB0ZDAx", Ts: i64(wall + 72_000 - 60_000 - 300),
			Kind: protocol.PushChargingInterrupted, Severity: SeverityWarning, Args: map[string]string{},
			Src: charger, LP: garage,
		}},
		{"event session complete", "box", MsgEvent, Event{
			Header: h(11, 73_000), EventID: "ZG9uZTAwMDAx", Ts: i64(wall + 73_000 - 60_000 - 200),
			Kind: protocol.PushChargingSessionComplete, Severity: SeverityInfo,
			Args: map[string]string{"kwh": "18.4"}, Src: charger, LP: garage,
		}},
		// The tunnel. A box linked at pairing, with the readable copy off:
		// two phones that may open a session, one QR code on the screen.
		{"roster tunnel only", "box", MsgRoster, Roster{
			Header: h(12, 74_000), RosterSeq: 5, TTLS: 86_400, Noise: bytes.Repeat([]byte{0x77}, 32),
			Devices: []Device{
				{ID: "q5Lw0Zb1", Key: bytes.Repeat([]byte{0x55}, 32), Role: "owner", Scopes: []string{}, Epoch: 1, Tun: true},
				{ID: "Zf09xQ2a", Key: bytes.Repeat([]byte{0x66}, 32), Role: "viewer", Scopes: []string{}, Epoch: 2, Tun: true},
			},
			Grant:   Grant{Scopes: []string{}, Purposes: []string{}, Controller: ControllerSourceful},
			Pending: []Pending{{Hash: PairingHash(HomeID(boxPK), bytes.Repeat([]byte{0x88}, 16)), TTLS: 600}},
		}},
		// A revoke that waits for its receipt: the grant keeps its id and
		// loses its scopes and purposes.
		{"roster revoking", "box", MsgRoster, Roster{
			Header: h(13, 75_000), RosterSeq: 6, TTLS: 86_400, Noise: bytes.Repeat([]byte{0x77}, 32),
			Devices: []Device{
				{ID: "q5Lw0Zb1", Key: bytes.Repeat([]byte{0x55}, 32), Role: "owner", Scopes: []string{}, Epoch: 1, Tun: true},
			},
			Grant: Grant{ID: bytes.Repeat([]byte{0x44}, GrantIDBytes), Scopes: []string{}, Purposes: []string{},
				Controller: ControllerSourceful},
		}},
		{"purged copy", "service", MsgPurged, Purged{AtMs: 1_791_821_100_000, What: PurgedCopy,
			GrantID: bytes.Repeat([]byte{0x44}, GrantIDBytes)}},
		{"tun down", "", MsgTun, Tun{Dev: "q5Lw0Zb1", Sid: 7, D: bytes.Repeat([]byte{0x99}, 112)}},
		{"tun up", "", MsgTun, Tun{Dev: "q5Lw0Zb1", Sid: 7, D: bytes.Repeat([]byte{0x9a}, 48)}},
		{"tun end", "", MsgTun, Tun{Dev: "q5Lw0Zb1", Sid: 7, D: []byte{}}},
		// An agent row: no tun and no copy scopes, because only the keys
		// part opens its session (docs/agents.md).
		{"roster with an agent", "box", MsgRoster, Roster{
			Header: h(14, 76_000), RosterSeq: 7, TTLS: 86_400, Noise: bytes.Repeat([]byte{0x77}, 32),
			Devices: []Device{
				{ID: "q5Lw0Zb1", Key: bytes.Repeat([]byte{0x55}, 32), Role: "owner", Scopes: []string{}, Epoch: 1, Tun: true},
				{ID: "oKGio6Sl", Key: bytes.Repeat([]byte{0xa0}, 32), Role: protocol.RoleSupport, Scopes: []string{},
					Epoch: 1, Agent: &Agent{
						Scopes: []string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead, protocol.ScopeLogsRead},
						Ops:    []string{protocol.OpLoadpointHold}, Caps: protocol.Caps{HoldS: 900, MaxW: 7400, PerMin: 6},
						ExpiresAtMs: 1_793_000_000_000,
						Principal:   protocol.Principal{Client: "Sourceful support", ClientID: protocol.SupportClientID, Ticket: "FTW-123"},
					}},
			},
			Grant: Grant{Scopes: []string{}, Purposes: []string{}, Controller: ControllerSourceful},
		}},
		// Cars (docs/cars.md). Two cars on one charger, neither named: the
		// box asks, the lp says so and offers the two, the car class
		// carries both levels, and car.which names the plug-in's session.
		{"lp car asking", "box", MsgLP, LP{
			Header: h(15, 76_000), ControlRev: 813,
			Loadpoints: []Loadpoint{{
				ID: garage, Src: charger, Plugged: true, SocSource: "unknown",
				PowerW: 0, SessionWh: 0, Session: "7d1f0c2a9b3e4f50", CommandedReason: "pv_surplus_pause",
				SurplusOnly: true, CarIdentifiedBy: protocol.CarIdentifiedByAsking, Choose: []string{tesla, volvo},
			}},
			Feedback: []Feedback{},
		}},
		{"car", "box", MsgCar, Cars{
			Header: h(16, 76_010),
			Cars: []Car{
				{ID: tesla, SocPermille: i64(140), SocSource: protocol.CarSoCVehicle, SocAgeS: i64(540),
					EnergySinceWh: 0, ChargeLimitPermille: i64(800),
					Goal: &CarGoal{TargetPermille: 800, ReadyByMin: 420, Days: 0x7f}},
				// Read before it last left a charger: an estimate, whatever
				// flowed since.
				{ID: volvo, SocPermille: i64(620), SocSource: protocol.CarSoCVehicle, SocAgeS: i64(30_600),
					EnergySinceWh: 0, SocEstimate: true, SurplusOnly: true},
			},
		}},
		{"event car which", "box", MsgEvent, Event{
			Header: h(17, 76_020), EventID: "d2hpY2hjYXIx", Ts: i64(wall + 76_020 - 60_000 - 100),
			Kind: protocol.PushCarWhich, Severity: SeverityInfo, Args: map[string]string{"session": "7d1f0c2a9b3e4f50"},
			Src: charger, LP: garage, Cars: []string{tesla, volvo},
		}},
		{"lp car tapped", "box", MsgLP, LP{
			Header: h(18, 81_000), ControlRev: 814,
			Loadpoints: []Loadpoint{{
				ID: garage, Src: charger, Plugged: true, SocPermille: i64(140), SocSource: "vehicle",
				SocAgeS: i64(545), TargetPermille: i64(800), ReadyByMs: i64(1_791_867_600_000),
				Recurring: true, Session: "7d1f0c2a9b3e4f50", CommandedReason: "plan",
				CarID: tesla, CarIdentifiedBy: protocol.CarIdentifiedByTap,
			}},
			Feedback: []Feedback{},
		}},
		{"roster browser notice", "box", MsgRoster, Roster{Header: h(19, 82_000), RosterSeq: 5, TTLS: 86400, Devices: []Device{{ID: "browser", Key: bytes.Repeat([]byte{0x55}, 32), Role: "owner", Scopes: []string{protocol.ScopeEventsRead}, Epoch: 1}}, WebPush: []WebPushRow{{ID: "opaque-subscription", Device: "browser", Epoch: 1, Endpoint: "https://push.example/opaque", CT: bytes.Repeat([]byte{0x66}, 128), Auth: "vapid box-signed"}}}},
		{"statistics service ready", "service", MsgHelloOK, HelloOK{BoxNonce: bytes.Repeat([]byte{0x33}, NonceBytes), EKM: bytes.Repeat([]byte{0x22}, EKMBytes), Have: map[string]string{}, RosterSeq: 4, StatsPolicy: StatsPolicy}},
		{"statistics closed hour", "box", MsgStats, Stats{Header: h(20, 83_000), StatsID: bytes.Repeat([]byte{0x88}, 16), Policy: StatsPolicy, HourMS: wall/StatsHourMS*StatsHourMS - StatsHourMS, Metrics: map[string]int64{"flexible_power_w": 11000, "pv_generation_wh": 120}}},
		{"statistics only roster", "box", MsgRoster, Roster{Header: h(23, 86_000), RosterSeq: 7, TTLS: 86400, Grant: Grant{ID: bytes.Repeat([]byte{0x44}, GrantIDBytes), Scopes: []string{}, Purposes: []string{protocol.CloudPurposeStats}, Controller: ControllerSourceful, TermsPolicy: StatsPolicy, StatsID: bytes.Repeat([]byte{0x88}, 16), StatsPolicy: StatsPolicy, StatsSinceMs: wall - 2*StatsHourMS}}},
		{"roster statistics consent", "box", MsgRoster, Roster{Header: h(21, 84_000), RosterSeq: 6, TTLS: 86400, Grant: Grant{TermsPolicy: CloudTermsPolicy, ID: bytes.Repeat([]byte{0x44}, GrantIDBytes), Purposes: []string{protocol.CloudPurposeServe, protocol.CloudPurposeStats}, Controller: ControllerSourceful, StatsID: bytes.Repeat([]byte{0x88}, 16), StatsPolicy: StatsPolicy, StatsSinceMs: wall - 2*StatsHourMS}}},
	}
}

func TestGoldenVectors(t *testing.T) {
	box := testKey(t, "ftw.cloud vector box")
	service := testKey(t, "ftw.cloud vector service")
	home, boxPK := homeOf(t, box)
	_, servicePK := homeOf(t, service)
	grantKey := bytes.Repeat([]byte{0x77}, GrantKeyBytes)

	var stored vectorFile
	raw, err := os.ReadFile(vectorsPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
	case !*update:
		t.Fatalf("%v (run with -update to create it)", err)
	}

	out := vectorFile{
		BoxKey:     vectorKey{D: hex.EncodeToString(box.D.FillBytes(make([]byte, 32))), PK: hex.EncodeToString(boxPK), Home: home},
		ServiceKey: vectorKey{D: hex.EncodeToString(service.D.FillBytes(make([]byte, 32))), PK: hex.EncodeToString(servicePK)},
		GrantKey:   hex.EncodeToString(grantKey),
	}
	for _, id := range []vectorID{{"src", "p1meter", ""}, {"src", "sungrow", ""}, {"src", "easee", ""}, {"lp", "garage", ""},
		{"car", vectorCarTesla, ""}, {"car", vectorCarVolvo, ""}} {
		switch id.Kind {
		case "src":
			id.ID = SourceID(grantKey, id.Name)
		case "car":
			id.ID = CarID(grantKey, id.Name)
		default:
			id.ID = LoadpointID(grantKey, id.Name)
		}
		out.IDs = append(out.IDs, id)
	}

	for i, v := range vectorBodies(grantKey, boxPK) {
		body, err := Marshal(v.body)
		if err != nil {
			t.Fatal(err)
		}
		input, err := SignedInput(home, v.t, body)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(input)
		vec := vectorSigned{Name: v.name, Signer: v.signer, T: v.t, Body: hex.EncodeToString(body),
			SignedInputSHA256: hex.EncodeToString(digest[:])}

		var sig []byte
		// A message whose signed input did not change keeps its stored
		// signature under -update, so a new vector does not re-sign the
		// rest of the file.
		unchanged := i < len(stored.Messages) && stored.Messages[i].Name == v.name &&
			stored.Messages[i].SignedInputSHA256 == vec.SignedInputSHA256
		switch {
		case v.signer != "" && *update && unchanged:
			sig, _ = hex.DecodeString(stored.Messages[i].Sig)
		case v.signer == "" || *update:
			if v.signer != "" {
				key := box
				if v.signer == "service" {
					key = service
				}
				if sig, err = Sign(rand.Reader, key, home, v.t, body); err != nil {
					t.Fatal(err)
				}
			}
		default:
			if i >= len(stored.Messages) || stored.Messages[i].Name != v.name {
				t.Fatalf("vector %d is not %q in %s; regenerate with -update", i, v.name, vectorsPath)
			}
			sig, _ = hex.DecodeString(stored.Messages[i].Sig)
		}
		env := Envelope{T: v.t, B: body, Sig: sig}
		if v.signer != "" {
			pub := &box.PublicKey
			if v.signer == "service" {
				pub = &service.PublicKey
			}
			if err := Verify(pub, home, env); err != nil {
				t.Fatalf("%s: stored signature does not verify: %v", v.name, err)
			}
		}
		raw, err := env.Encode()
		if err != nil {
			t.Fatal(err)
		}
		frame, err := EncodeFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		vec.Sig, vec.Envelope, vec.Frame = hex.EncodeToString(sig), hex.EncodeToString(raw), hex.EncodeToString(frame)
		out.Messages = append(out.Messages, vec)

		// What decodes from the frame is what was encoded.
		payload, err := DecodeFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		back, err := DecodeEnvelope(payload)
		if err != nil || back.T != v.t || !bytes.Equal(back.B, body) || !bytes.Equal(back.Sig, sig) {
			t.Fatalf("%s: round trip: %v", v.name, err)
		}
	}

	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, _ := os.ReadFile(vectorsPath)
	if !bytes.Equal(got, want) {
		t.Fatalf("the bytes no longer match %s. If the wire change is intended, "+
			"regenerate with -update and pair the change with srcfl/ftw-cloud", vectorsPath)
	}
}
