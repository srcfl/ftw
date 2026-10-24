package cloudwire

import (
	"errors"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/srcfl/ftw/go/protocol"
)

// Message types. The service sends exactly the first five and tun; anything
// else from it closes the uplink.
const (
	MsgChallenge = "challenge"
	MsgHelloOK   = "hello_ok"
	MsgWatch     = "watch"
	MsgResync    = "resync"
	MsgPurged    = "purged"
)

// Message types the box sends.
const (
	MsgHello  = "hello"
	MsgLive   = "live"
	MsgLP     = "lp"
	MsgPrices = "prices"
	MsgPlan   = "plan"
	MsgHealth = "health"
	MsgEvent  = "event"
	MsgTiles  = "tiles"
	MsgHB     = "hb"
	MsgRoster = "roster"
	MsgBye    = "bye"
	MsgCar    = "car"
	// MsgNotice tells the owners' phones that something changed who may
	// reach the box (docs/agents.md, Push kinds). It carries the kind and
	// nothing else, and the box sends it whenever it is linked, copy or not.
	MsgNotice = "notice"
)

// Downlink is the closed set of types the service may send the box. None of
// the first five asks the box to act. tun and x carry ciphertext the box
// decrypts itself and judges as it judges the same session anywhere else.
var Downlink = []string{MsgChallenge, MsgHelloOK, MsgWatch, MsgResync, MsgPurged, MsgTun, MsgX}

// ClassScope is the scope a device needs to read each class the service
// forwards. hb maps to "" because every device in the roster gets it: it
// carries no readings, only how old they are.
var ClassScope = map[string]string{
	MsgLive:   protocol.ScopeLiveRead,
	MsgLP:     protocol.ScopeLiveRead,
	MsgCar:    protocol.ScopeLiveRead,
	MsgPrices: protocol.ScopeLiveRead,
	MsgPlan:   protocol.ScopePlanRead,
	MsgHealth: protocol.ScopeHealthRead,
	MsgEvent:  protocol.ScopeEventsRead,
	MsgTiles:  protocol.ScopeHistoryRead,
	MsgHB:     "",
}

// ServiceOnly are box messages the service acts on and never forwards.
var ServiceOnly = []string{MsgHello, MsgRoster, MsgBye, MsgNotice, MsgStats}

// Clock values for hello.clock and hb.clock.
const (
	ClockNTP  = "ntp"
	ClockNone = "none"
)

// Bye reasons.
const (
	ByeRevoked  = "revoked"
	ByeShutdown = "shutdown"
	ByeUpdate   = "update"
)

// What a purge receipt or a resync names. PurgedCopy confirms that the
// readable copy of a grant is gone while the home stays, for its tunnel.
const (
	PurgedHome  = "home"
	PurgedCopy  = "copy"
	PurgedStats = "stats"

	ResyncFull   = "full"
	ResyncTiles  = "tiles"
	ResyncRoster = "roster"
)

// ControllerSourceful is the data controller unless a partner flow names
// another. No such flow exists yet.
const ControllerSourceful = "sourceful"

// --------------------------------------------------------------------------
// Service to box
// --------------------------------------------------------------------------

// Challenge is the service's first message, unsigned.
type Challenge struct {
	V     int    `cbor:"v"`
	Nonce []byte `cbor:"nonce"`
}

// HelloOK answers hello. The service signs it, and the box sends nothing but
// hello before it has checked it.
type HelloOK struct {
	StatsPolicy string `cbor:"statsPolicy,omitempty"`
	BoxNonce    []byte `cbor:"boxNonce"`
	// EKM is the service's TLS exporter value for this session. A proxy that
	// ends TLS holds two sessions with two values, so the check fails there.
	EKM []byte `cbor:"ekm"`
	// Have maps tile id to etag for the tiles the service holds.
	Have      map[string]string `cbor:"have"`
	RosterSeq uint64            `cbor:"rosterSeq"`
}

// Watch raises the period to one second for 60 seconds.
type Watch struct {
	Hz int `cbor:"hz"`
}

// Resync asks for full state again. The box honours one per five minutes.
type Resync struct {
	What  []string `cbor:"what"`
	Tiles []string `cbor:"tiles,omitempty"`
}

// Purged is the service's signed receipt that a deletion is done.
type Purged struct {
	AtMs    int64  `cbor:"atMs"`
	What    string `cbor:"what"`
	GrantID []byte `cbor:"grantId"`
	StatsID []byte `cbor:"statsId,omitempty"`
}

// --------------------------------------------------------------------------
// Box to service
// --------------------------------------------------------------------------

// Header is in every body the box sends.
type Header struct {
	// Seq starts at 1 and rises by 1 per message, per boot, across
	// reconnects.
	Seq uint64 `cbor:"seq"`
	// Boot is drawn at random at each Core start.
	Boot []byte `cbor:"boot"`
	// UptimeMs is the box's monotonic time since start, at send.
	UptimeMs int64 `cbor:"uptimeMs"`
	// WallMs is the box's wall time at send, present only while NTP has
	// synced the box clock.
	WallMs *int64 `cbor:"wallMs,omitempty"`
}

// Hello proves the box holds its key and binds this TLS session to it.
type Hello struct {
	Header
	V int `cbor:"v"`
	// PK is the box's public key, 65 bytes. The home id is a hash of it.
	PK       []byte `cbor:"pk"`
	Nonce    []byte `cbor:"nonce"`
	EKM      []byte `cbor:"ekm"`
	BoxNonce []byte `cbor:"boxNonce"`
	Build    string `cbor:"build"`
	Clock    string `cbor:"clock"`
	// Caps lists the classes this box sends.
	Caps      []string `cbor:"caps"`
	RosterSeq uint64   `cbor:"rosterSeq"`
	GrantID   []byte   `cbor:"grantId"`
}

// Live is the site's flows and the freshness of every source behind them.
type Live struct {
	Header
	// Fields uses the app protocol's frozen ids 1–6 and 10, and its extra
	// fields 11–14 when the site meter gives phases, keyed and encoded as its
	// delta does.
	Fields map[string]int64 `cbor:"fields"`
	// FieldSources and Sources travel only when the source table changed and
	// in every full message.
	FieldSources map[string][]string `cbor:"fieldSources,omitempty"`
	Sources      map[string]Source   `cbor:"sources,omitempty"`
	// SrcRev rises by 1, per boot, each time the source table changes.
	SrcRev uint64 `cbor:"srcRev"`
	// Ages is, for each live or lagging source, the age of its newest
	// reading at send, in ms of box uptime.
	Ages              map[string]int64 `cbor:"ages"`
	DispatchBlockedBy []string         `cbor:"dispatchBlockedBy"`
}

// Source is one source's freshness as the box grades it.
type Source struct {
	Kind         string               `cbor:"kind"`
	State        protocol.SourceState `cbor:"state"`
	LastOkMs     int64                `cbor:"lastOkMs"`
	StaleAfterMs int64                `cbor:"staleAfterMs"`
}

// LP is loadpoint and control feedback state.
type LP struct {
	Header
	ControlRev uint64      `cbor:"controlRev"`
	Loadpoints []Loadpoint `cbor:"loadpoints"`
	Feedback   []Feedback  `cbor:"feedback"`
}

// Loadpoint is one charger as the household sees it. Capabilities such as
// maximum power or phases are not sent.
type Loadpoint struct {
	ID              string  `cbor:"id"`
	Src             string  `cbor:"src"`
	Plugged         bool    `cbor:"plugged"`
	SocPermille     *int64  `cbor:"socPermille"`
	SocSource       string  `cbor:"socSource"`
	SocAgeS         *int64  `cbor:"socAgeS"`
	TargetPermille  *int64  `cbor:"targetPermille"`
	ReadyByMs       *int64  `cbor:"readyByMs"`
	Recurring       bool    `cbor:"recurring"`
	PowerW          int64   `cbor:"powerW"`
	SessionWh       int64   `cbor:"sessionWh"`
	Session         string  `cbor:"session"`
	Manual          *Manual `cbor:"manual"`
	CommandedW      *int64  `cbor:"commandedW"`
	CommandedReason string  `cbor:"commandedReason"`
	SurplusOnly     bool    `cbor:"surplusOnly"`
	GridDeferred    bool    `cbor:"gridDeferred"`
	PlanPending     bool    `cbor:"planPending"`
	Boost           bool    `cbor:"boost"`
	NextStartMs     *int64  `cbor:"nextStartMs"`
	NextEndMs       *int64  `cbor:"nextEndMs"`
	NextWh          *int64  `cbor:"nextWh"`
	// The car on the charger, by opaque car id, and how the box knows it
	// (docs/cars.md). Absent while nothing is plugged in, and from a box
	// without cars. The level fields above are views of this car for one
	// release; the car class is the truth.
	CarID            string   `cbor:"carId,omitempty"`
	CarIdentifiedBy  string   `cbor:"carIdentifiedBy,omitempty"`
	CarCorrected     bool     `cbor:"carCorrected,omitempty"`
	CarCorrectedFrom string   `cbor:"carCorrectedFrom,omitempty"`
	Choose           []string `cbor:"choose,omitempty"`
}

// Cars is the car class: the level and goal of each of the owners' cars on
// this box. Nothing in it names a car: the id is opaque, and the phone reads
// the name, maker, VIN and sources from the box over Noise.
type Cars struct {
	Header
	Cars []Car `cbor:"cars"`
}

// Car is one car's level and goal.
type Car struct {
	// ID is the opaque car id.
	ID          string `cbor:"id"`
	SocPermille *int64 `cbor:"socPermille"`
	SocSource   string `cbor:"socSource"`
	// SocAgeS is the seconds since the car reported the level or someone
	// set it, at send. Null without a level.
	SocAgeS *int64 `cbor:"socAgeS"`
	// EnergySinceWh is negative when a V2X session took energy out.
	EnergySinceWh       int64    `cbor:"energySinceWh"`
	SocEstimate         bool     `cbor:"socEstimate"`
	ChargeLimitPermille *int64   `cbor:"chargeLimitPermille"`
	Goal                *CarGoal `cbor:"goal"`
	SurplusOnly         bool     `cbor:"surplusOnly"`
}

// CarGoal is protocol.CarGoal, as the car class carries it.
type CarGoal struct {
	TargetPermille int64 `cbor:"targetPermille"`
	ReadyByMin     int64 `cbor:"readyByMin"`
	Days           uint8 `cbor:"days"`
	FinishAtLimit  bool  `cbor:"finishAtLimit"`
}

// Manual is a manual charging hold.
type Manual struct {
	State      string `cbor:"state"`
	SinceAgeMs int64  `cbor:"sinceAgeMs"`
	RequestedW *int64 `cbor:"requestedW"`
	CommandedW *int64 `cbor:"commandedW"`
	Reason     string `cbor:"reason"`
}

// Feedback is Core's verdict on whether one device function follows.
type Feedback struct {
	Kind     string `cbor:"kind"`
	Src      string `cbor:"src"`
	Status   string `cbor:"status"`
	Reason   string `cbor:"reason"`
	Severity string `cbor:"severity"`
	Evidence string `cbor:"evidence"`
	Fresh    bool   `cbor:"fresh"`
	ActualW  *int64 `cbor:"actualW"`
	AgeMs    *int64 `cbor:"ageMs"`
}

// Prices is the price window, in columns.
type Prices struct {
	Header
	Zone     string `cbor:"zone"`
	Currency string `cbor:"currency"`
	T0Ms     int64  `cbor:"t0Ms"`
	SlotMs   int64  `cbor:"slotMs"`
	// SpotMinor and TotalMinor are minor units per kWh, null where a slot
	// has no price. TotalMinor includes tariff and VAT.
	SpotMinor  []*int64 `cbor:"spotMinor"`
	TotalMinor []*int64 `cbor:"totalMinor"`
}

// HB says the box is there and how old its readings are. It never vouches
// for a live or lp the box has not sent: it names the newest of each.
type HB struct {
	Header
	Clock   string           `cbor:"clock"`
	SrcRev  uint64           `cbor:"srcRev"`
	LiveSeq uint64           `cbor:"liveSeq"`
	LpSeq   uint64           `cbor:"lpSeq"`
	Ages    map[string]int64 `cbor:"ages"`
}

// Roster is every device paired with the box and what the owner granted.
// The service learns devices from nothing else.
type Roster struct {
	Header
	RosterSeq uint64   `cbor:"rosterSeq"`
	TTLS      int64    `cbor:"ttlS"`
	Devices   []Device `cbor:"devices"`
	Grant     Grant    `cbor:"grant"`
	// Pending lists phone pairing and agent invitation hints. Each kind
	// has its own admission path; agent hints never pair a phone.
	Pending []Pending `cbor:"pending"`
	// Noise is the box's X25519 Noise static key, the one in its pairing
	// QR code. FTW Cloud's keys part opens an agent's session to it, and
	// trusts it only as signed here, by the home's key.
	Noise []byte `cbor:"noise"`
	// WebPush holds browser farewells encrypted and signed by the box.
	// The service never forwards these rows to a phone.
	WebPush []WebPushRow `cbor:"webpush,omitempty"`
}

// WebPushRow is a pre-encrypted box.unreachable notice for one browser.
// Epoch binds it to the current device grant. No push key leaves the box.
type WebPushRow struct {
	ID       string `cbor:"id"`
	Device   string `cbor:"dev"`
	Epoch    uint64 `cbor:"epoch"`
	Endpoint string `cbor:"endpoint"`
	CT       []byte `cbor:"ct"`
	Auth     string `cbor:"auth"`
}

// Device is one paired phone.
type Device struct {
	ID string `cbor:"id"`
	// Key is the device's X25519 Noise static key.
	Key  []byte `cbor:"key"`
	Role string `cbor:"role"`
	// Scopes are what the box derives from the role. The service holds no
	// role table.
	Scopes []string `cbor:"scopes"`
	Epoch  uint64   `cbor:"epoch"`
	// Tun says the device may open a session to the box through the
	// service. It needs no grant: the readable copy is a purpose on top.
	Tun bool `cbor:"tun"`
	// Agent is set on an agent row: an assistant, Sourceful support or the
	// release check (docs/agents.md). Absent on a phone or a watch.
	Agent *Agent `cbor:"agent,omitempty"`
}

// Agent is an agent row's grant, as the box enforces it. The service must
// stop admitting the row at ExpiresAtMs by its own clock, never admit it
// through the relay, and may tighten the caps but never loosen them.
type Agent struct {
	// Scopes and Ops are the grant at the approving owner's scopes now.
	Scopes      []string           `cbor:"scopes"`
	Ops         []string           `cbor:"ops"`
	Caps        protocol.Caps      `cbor:"caps"`
	ExpiresAtMs int64              `cbor:"expiresAtMs"`
	Principal   protocol.Principal `cbor:"principal"`
}

// Pending is a bootstrap hint. Kind keeps phone pairing and agent claims apart.
type Pending struct {
	// Kind is empty for phone pairing; agent invitations never pair a phone.
	Kind string `cbor:"kind,omitempty"`
	Hash []byte `cbor:"hash"`
	// TTLS is how long the code has left, counted from when the service
	// receives the roster.
	TTLS int64 `cbor:"ttlS"`
}

// Grant is what the owner turned on. With the readable copy off it has no
// id, scopes or purposes. While a revoke waits for its receipt it keeps the
// id and has neither scopes nor purposes.
type Grant struct {
	TermsPolicy  string   `cbor:"termsPolicy,omitempty"`
	ID           []byte   `cbor:"id"`
	Scopes       []string `cbor:"scopes"`
	Purposes     []string `cbor:"purposes"`
	Controller   string   `cbor:"controller"`
	StatsID      []byte   `cbor:"statsId,omitempty"`
	StatsPolicy  string   `cbor:"statsPolicy,omitempty"`
	StatsSinceMs int64    `cbor:"statsSinceMs,omitempty"`
}

// Notice is a security notice for the owners: a push kind from the
// catalogue, such as agent.added, and nothing that says more. The service
// sends each owner device registered for alerts a push carrying the
// notice, which the phone checks and words from its catalogue.
type Notice struct {
	Header
	Kind string `cbor:"kind"`
	// Client is the name of the client given access, as FTW Cloud's
	// consent page showed it: only on agent.added, and only while the
	// readable copy is on and carries events. Otherwise absent.
	Client string `cbor:"client,omitempty"`
}

// NoticeKinds are the kinds a notice may carry.
var NoticeKinds = []string{protocol.PushAgentAdded, protocol.PushAgentRequest, protocol.PushAgentExpiring,
	protocol.PushApprovalKeySet, protocol.PushOwnerAdded}

// NoticeClientMaxRunes bounds a notice's client name in characters: FTW
// Cloud's consent page keeps at most 64 of a name and marks one it cut with
// "…", so every name it shows fits.
const NoticeClientMaxRunes = 65

// NoticeMaxAgeMs is how old a notice may be and still get its words on the
// phone, and how long Apple may keep trying to deliver it. A notice states
// a lasting fact, such as a new owner, so it lasts longer than an event.
const NoticeMaxAgeMs = 24 * 60 * 60 * 1000

// NoticeClientOK reports whether a client name may go in a notice: valid
// UTF-8, at most NoticeClientMaxRunes characters, and no control or format
// character, which could reorder or hide what a lock screen shows.
func NoticeClientOK(c string) bool {
	if c == "" || !utf8.ValidString(c) || utf8.RuneCountInString(c) > NoticeClientMaxRunes {
		return false
	}
	for _, r := range c {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// ErrNotice refuses a notice body that is not one: an unknown field, a
// header out of shape, a kind outside NoticeKinds, or a client name where
// none may be or that NoticeClientOK refuses.
var ErrNotice = errors.New("cloudwire: not a notice")

// ReadNotice decodes a notice body strictly. A body with any field beyond
// the header, kind and client is refused, so a notice cannot carry a
// reading, a name or an id to a phone or to Apple.
func ReadNotice(body []byte) (Notice, error) {
	var keys map[string]cbor.RawMessage
	if Unmarshal(body, &keys) != nil {
		return Notice{}, ErrNotice
	}
	for k := range keys {
		if !slices.Contains(Fields["header"], k) && !slices.Contains(Fields[MsgNotice], k) {
			return Notice{}, ErrNotice
		}
	}
	var n Notice
	if Unmarshal(body, &n) != nil || len(n.Boot) != BootBytes || n.UptimeMs < 0 || !slices.Contains(NoticeKinds, n.Kind) ||
		(n.Client != "" && (n.Kind != protocol.PushAgentAdded || !NoticeClientOK(n.Client))) {
		return Notice{}, ErrNotice
	}
	return n, nil
}

// Bye ends the uplink. Purge asks the service to delete the home.
type Bye struct {
	Header
	Why   string `cbor:"why"`
	Purge bool   `cbor:"purge"`
}
