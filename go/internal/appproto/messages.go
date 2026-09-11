package appproto

import "github.com/srcfl/ftw/go/internal/control"

// Message type strings. Stable for the life of the protocol.
const (
	MsgHello            = "hello"
	MsgHelloOK          = "hello_ok"
	MsgSub              = "sub"
	MsgSnap             = "snap"
	MsgDelta            = "delta"
	MsgTick             = "tick"
	MsgPlanGet          = "plan.get"
	MsgPlan             = "plan"
	MsgCmd              = "cmd"
	MsgCmdAck           = "cmd.ack"
	MsgCmdResult        = "cmd.result"
	MsgEvent            = "event"
	MsgError            = "error"
	MsgSessionTerminate = "session.terminate"
	MsgPriceGet         = "price.get"
	MsgPrice            = "price"
	MsgHistQuery        = "hist.query"
	MsgHistChunk        = "hist.chunk"
	MsgHistEnd          = "hist.end"
	MsgAPIReq           = "api.req"
	MsgAPIHead          = "api.head"
	MsgAPIChunk         = "api.chunk"
	MsgAPIEnd           = "api.end"
)

// Operations a client may ask for, from contract/registry.yaml `ops` — the
// scope each demands lives there too, and TestRegistryOpsMatchCommandTable
// reads the pair back against defaultOps().
const (
	// OpSetMode changes the site operating mode. It goes through the same
	// mode validation the API and Home Assistant use — a second validator
	// here would be a second place for the two to disagree.
	OpSetMode = "site.mode.set"
	// OpLoadpointHold pins one EV loadpoint to a fixed charging power, or
	// releases it with `clear`. The same manual hold the HTTP route
	// installs, reached through this door's gates instead of a verb.
	OpLoadpointHold = "loadpoint.hold"
	// OpLoadpointBoost lets the house battery boost the car under a bounded
	// lease, or withdraws it with `cancel`. Same lease and same preflight
	// as the HTTP route.
	OpLoadpointBoost = "loadpoint.boost"
	// OpLoadpointSoCSet corrects the car's current state of charge, a
	// fraction in [0,1], the way POST /api/loadpoints/{id}/soc does: the
	// session's estimate is re-anchored and the plan remade from it.
	OpLoadpointSoCSet = "loadpoint.soc.set"
	// OpLoadpointSurplusOnlySet turns PV-only charging on or off for one
	// loadpoint — the `surplus_only` field of the HTTP target route, and
	// only that field: the target level and its deadline have no command.
	OpLoadpointSurplusOnlySet = "loadpoint.surplus_only.set"
)

// BoxMode is what the box is able to offer this session.
type BoxMode string

const (
	// BoxModeFull — everything the box supports.
	BoxModeFull BoxMode = "full"
	// BoxModeFloor — the frozen subset, because the app is too old.
	BoxModeFloor BoxMode = "floor"
	// BoxModeBooting — still starting; no subscription yet.
	BoxModeBooting BoxMode = "booting"
	// BoxModeReadonly — the grant carries no write scope.
	BoxModeReadonly BoxMode = "readonly"
)

// --------------------------------------------------------------------------
// Handshake
// --------------------------------------------------------------------------

// Hello is the client's opening message.
type Hello struct {
	// Proto is a range, never an equality demand. That is what avoids a
	// version wall.
	Proto   ProtoRange `cbor:"proto"`
	App     AppInfo    `cbor:"app"`
	Locales []string   `cbor:"locales"`
	// Sub folds the first telemetry subscription into the hello. Older boxes
	// ignore this additive field; subscribed in hello_ok tells a newer app whether
	// it still needs to send the ordinary sub message.
	Sub *Sub `cbor:"sub,omitempty"`
}

type ProtoRange struct {
	Min int `cbor:"min"`
	Max int `cbor:"max"`
}

type AppInfo struct {
	Build string `cbor:"build"`
	UA    string `cbor:"ua"`
}

// HelloOK answers it.
//
// Sent on the bulk lane, not lane 0. It carries the capability list and the
// mode catalogue, both of which vary in size with what the box supports, and
// lane 0 exists precisely so that its frames do not vary in size with
// anything. A handshake already announces itself by happening, so a variable
// length here leaks nothing that was not already visible.
type HelloOK struct {
	// Proto is the negotiated version. ProtoFloor means the app is too old
	// for full mode and should degrade to the frozen subset.
	Proto int     `cbor:"proto"`
	Mode  BoxMode `cbor:"mode"`
	Box   BoxInfo `cbor:"box"`
	Clock Clock   `cbor:"clock"`
	// Caps is presence, not levels. Absent means hide the feature.
	Caps     []string `cbor:"caps"`
	CapsHash string   `cbor:"capsHash"`
	// Modes is every mode this box accepts, in its own presentation order,
	// straight from control.ModeCatalog(). Field 1 in the telemetry stream
	// carries an index into this list, which is what keeps lane 0 numeric
	// and its frames a fixed size.
	Modes []ModeInfo `cbor:"modes"`
	// Boot is present only while the box is still starting. A VACUUM can
	// take minutes; saying so beats a spinner that looks hung.
	Boot *BootProgress `cbor:"boot,omitempty"`
	Hint string        `cbor:"hint,omitempty"`
	// Role and Scopes are what this enrolment holds. The app uses them for
	// one thing: deciding what to draw. The app hiding a button is
	// presentation — if the app is wrong and shows it, the box refuses it.
	//
	// Both are sent, and the redundancy is deliberate. Role alone would make
	// the app read a role table it has no reason to hold; the expanded list
	// alone would leave the app unable to say "viewer" in a sentence.
	Role   string   `cbor:"role"`
	Scopes []string `cbor:"scopes"`
	// Subscribed says the optional subscription carried by Hello was accepted.
	// False also covers an older box, where this additive field is absent.
	Subscribed bool `cbor:"subscribed,omitempty"`
}

// HintAppUpdate tells the app it is behind. It is a hint, not an error: the
// session still works, on the frozen subset.
const HintAppUpdate = "app_update"

type BoxInfo struct {
	ID    string `cbor:"id"`
	Build string `cbor:"build"`
	TZ    string `cbor:"tz"`
}

// Clock is how the app anchors every age it renders.
//
// SyncedAtMs is wall clock and may be nonsense before NTP answers; UptimeMs
// never is, which is why it and not the wall clock is what ages are measured
// against.
type Clock struct {
	Source     string `cbor:"source"`
	SyncedAtMs int64  `cbor:"syncedAtMs"`
	UptimeMs   int64  `cbor:"uptimeMs"`
}

// ModeInfo is one mode as the box presents it. The wire shape of
// control.ModeInfo.
type ModeInfo struct {
	Key     string `cbor:"key"`
	Label   string `cbor:"label"`
	Tooltip string `cbor:"tooltip"`
	Tier    string `cbor:"tier"`
}

// BootProgress is what the box is busy with before it can serve telemetry.
type BootProgress struct {
	Phase string `cbor:"phase"`
	Pct   int    `cbor:"pct"`
	// EtaMs is nil when the box genuinely cannot estimate. A made-up number
	// here is worse than none.
	EtaMs *int64 `cbor:"etaMs"`
}

// Boot phases.
const (
	BootPhaseVacuum  = "vacuum"
	BootPhaseMigrate = "migrate"
	BootPhaseDrivers = "drivers"
)

// --------------------------------------------------------------------------
// Telemetry
// --------------------------------------------------------------------------

// Sub starts the telemetry stream.
type Sub struct {
	// Bucket is the lane 0 frame size, fixed by the first subscription: 256 or
	// 512. A later sub may change only Hz.
	Bucket int `cbor:"bucket"`
	// Hz is the cadence: 1 in the foreground, 0.2 when the document is hidden.
	// The app sends another sub when its visibility changes.
	Hz float64 `cbor:"hz"`
}

// SourceWire is one device's freshness.
//
// Readings point at a source rather than carrying their own timestamp. That
// keeps a delta at tens of bytes while still answering the question people
// actually have: the box is fine, but the inverter went quiet 40 seconds ago.
type SourceWire struct {
	Kind string `cbor:"kind"`
	Name string `cbor:"name"`
	// LastOkMs is box uptime at the last good reading, not wall clock.
	LastOkMs     int64       `cbor:"lastOkMs"`
	StaleAfterMs int64       `cbor:"staleAfterMs"`
	State        SourceState `cbor:"state"`
}

// FieldDef is one entry of the field dictionary. The client caches it across
// sessions, which is why deltas can be bare id-to-value pairs.
// Unit and SrcID are pointers so that "this field has none" reaches the app as
// CBOR null rather than an empty string. The app's type says `string | null`,
// and a field whose source is the empty string would look like a source id
// nobody can resolve.
type FieldDef struct {
	Name string  `cbor:"name"`
	Unit *string `cbor:"unit"`
	// SrcID is the source this field's freshness comes from. Null for
	// fields the box computes itself, such as the mode.
	SrcID *string `cbor:"srcId"`
}

// Snap is a full site snapshot. Bulk lane: it carries the dictionary and every
// source, so its size depends on how many devices the site has.
type Snap struct {
	UptimeMs int64 `cbor:"uptimeMs"`
	// ControlRev is monotonic per site, bumped by every mutation of
	// controllable state. A command carries the rev it expected.
	ControlRev uint64                `cbor:"controlRev"`
	Dict       map[string]FieldDef   `cbor:"dict"`
	Fields     map[string]int64      `cbor:"fields"`
	Sources    map[string]SourceWire `cbor:"sources"`
	// DispatchBlockedBy names the sources currently stopping dispatch. It
	// connects to the box's own invariant that stale meter data stops
	// dispatch, so the app can say why nothing is happening rather than
	// looking broken.
	DispatchBlockedBy []string `cbor:"dispatchBlockedBy"`
}

// Delta carries only what moved.
type Delta struct {
	Seq      uint64           `cbor:"seq"`
	UptimeMs int64            `cbor:"uptimeMs"`
	Fields   map[string]int64 `cbor:"fields"`
	// Sources is usually absent, because source state changes rarely. But
	// when a device goes quiet mid-session this is the only way the client
	// hears about it, so "rarely" must never become "only in the snapshot".
	Sources map[string]SourceWire `cbor:"sources,omitempty"`
	// DispatchBlockedBy travels with Sources; the two always move together.
	DispatchBlockedBy []string `cbor:"dispatchBlockedBy,omitempty"`
}

// Tick says nothing changed.
//
// Sent anyway, on the same cadence and in the same bucket. Silence would
// itself be information: it would tell the relay operator that nothing
// happened in the house that second.
type Tick struct {
	Seq      uint64 `cbor:"seq"`
	UptimeMs int64  `cbor:"uptimeMs"`
}

// --------------------------------------------------------------------------
// Commands
// --------------------------------------------------------------------------

// Guard is a precondition re-evaluated against fresh state at the dispatch
// boundary, not against the state the user was looking at.
type Guard struct {
	Fid   Fid     `cbor:"fid"`
	Op    string  `cbor:"op"`
	Value float64 `cbor:"value"`
}

// Guard comparison operators.
const (
	GuardLT  = "lt"
	GuardLTE = "lte"
	GuardGT  = "gt"
	GuardGTE = "gte"
)

// Cmd is an intent, not an instruction to hardware.
type Cmd struct {
	// CmdID is a UUIDv7 idempotency key. The box keeps it 24 h so a retry
	// cannot act twice.
	CmdID string         `cbor:"cmdId"`
	Op    string         `cbor:"op"`
	Args  map[string]any `cbor:"args"`
	// NotValidAfterMs is mandatory, in box uptime. A command queued offline
	// must never execute blindly later: "charge at 10 kW" arriving three
	// hours late is a different instruction than the one the user gave.
	NotValidAfterMs int64  `cbor:"notValidAfterMs"`
	Expect          Expect `cbor:"expect"`
}

type Expect struct {
	Rev    uint64  `cbor:"rev"`
	Guards []Guard `cbor:"guards"`
}

// CmdAck says the dispatcher accepted the intent. It is not proof the hardware
// moved.
type CmdAck struct {
	CmdID   string `cbor:"cmdId"`
	LeaseID string `cbor:"leaseId"`
	// ExpiresAtMs is absolute box uptime.
	ExpiresAtMs int64 `cbor:"expiresAtMs"`
}

// Command outcomes.
const (
	CmdApplied     = "applied"
	CmdRejected    = "rejected"
	CmdExpired     = "expired"
	CmdSuperseded  = "superseded"
	CmdUnconfirmed = "unconfirmed"
)

// CmdResult is what actually happened.
//
// Observed is present only when the driver read the value back. The echo of a
// requested value is never sent as confirmation — that is the whole difference
// between knowing a command was heard and knowing it happened.
type CmdResult struct {
	CmdID    string     `cbor:"cmdId"`
	State    string     `cbor:"state"`
	Observed *Observed  `cbor:"observed,omitempty"`
	Error    *ErrorBody `cbor:"error,omitempty"`
}

type Observed struct {
	Value float64 `cbor:"value"`
	Src   string  `cbor:"src"`
	// UptimeMs is when the value was read back.
	UptimeMs int64 `cbor:"uptimeMs"`
}

// ObservedSrcCore names the box itself as the source of a read-back value, for
// state the box holds rather than a device reports.
const ObservedSrcCore = "core"

// --------------------------------------------------------------------------
// Plan
// --------------------------------------------------------------------------

// PlanReason is why a slot looks the way it does.
//
// A stable code, never prose: the box has no idea what language anyone reads,
// and the app owns every word the user sees.
type PlanReason string

const (
	ReasonCheapImport     PlanReason = "cheap_import"
	ReasonExpensiveImport PlanReason = "expensive_import"
	ReasonSolarSurplus    PlanReason = "solar_surplus"
	ReasonPeakShaving     PlanReason = "peak_shaving"
	ReasonReserveHeld     PlanReason = "reserve_held"
	ReasonExportPaid      PlanReason = "export_paid"
	ReasonIdle            PlanReason = "idle"
)

// PlanSlot is one settlement slot of intent.
type PlanSlot struct {
	// StartMs is wall clock, because these are future times a person reads.
	StartMs int64 `cbor:"startMs"`
	// DurationMs is 15 minutes on the box today. Do not assume it.
	DurationMs int64 `cbor:"durationMs"`
	// BatteryW and GridW follow the site convention: positive is power into
	// the site, so a positive BatteryW charges and a negative GridW exports.
	BatteryW int64 `cbor:"batteryW"`
	GridW    int64 `cbor:"gridW"`
	// PriceMinor is the import price in minor units per kWh, nil when the
	// slot's price is not known.
	PriceMinor *int64     `cbor:"priceMinor"`
	Reason     PlanReason `cbor:"reason"`
}

// Plan is what the box intends to do.
//
// Intent, not prophecy. The box revalidates against fresh state every tick, so
// a plan is what it currently means to do.
type Plan struct {
	Rev uint64 `cbor:"rev"`
	// UptimeMs is box uptime when the plan was made.
	UptimeMs int64      `cbor:"uptimeMs"`
	Slots    []PlanSlot `cbor:"slots"`
	// Stale is set when the planner could not run and the box fell back.
	// "Nothing is scheduled" and "we do not know what is scheduled" are
	// different sentences and only one of them would be honest here.
	Stale bool `cbor:"stale"`
	// CeilingW is the import ceiling being defended, nil when there is none.
	CeilingW *int64 `cbor:"ceilingW"`
}

// --------------------------------------------------------------------------
// Prices
// --------------------------------------------------------------------------

// PriceGet asks for the price slots covering a window.
type PriceGet struct {
	// FromMs and ToMs are wall clock: prices are about hours a person plans
	// around, not about box uptime.
	FromMs int64 `cbor:"fromMs"`
	ToMs   int64 `cbor:"toMs"`
}

// PriceSlot is one settlement slot's price.
//
// Minor units per kWh (öre, cents) as integers, because a price is money and
// money in a float is a rounding argument waiting to happen. Spot is the raw
// market price; Total is what the household actually pays, tariff and tax
// included — the box computes it because the box holds the configuration.
type PriceSlot struct {
	StartMs    int64 `cbor:"startMs"`
	DurationMs int64 `cbor:"durationMs"`
	SpotMinor  int64 `cbor:"spotMinor"`
	TotalMinor int64 `cbor:"totalMinor"`
}

// Price is the answer to PriceGet.
type Price struct {
	// Zone and Currency label the numbers. Without them the app would have to
	// guess whether 45 is öre or cents.
	Zone     string      `cbor:"zone"`
	Currency string      `cbor:"currency"`
	Slots    []PriceSlot `cbor:"slots"`
	// Stale means these slots do not cover the window that was asked for:
	// they begin after its start, stop short of its end, or there is a hole
	// in the middle. Nothing in this path knows whether a refresh failed,
	// only whether the window came back whole. Tomorrow's rates arrive in the
	// afternoon, so a window asked for at breakfast genuinely ends early, and
	// saying so beats drawing a cliff the market did not have.
	Stale bool `cbor:"stale"`
}

// --------------------------------------------------------------------------
// History
// --------------------------------------------------------------------------

// HistQuery asks for stored series over a window.
type HistQuery struct {
	// Series are frozen field names — grid_w, pv_w, battery_w, load_w.
	Series []string `cbor:"series"`
	// Res is the resolution asked for. The box may serve coarser and says so
	// in HistEnd.ResActual.
	Res Resolution `cbor:"res"`
	// FromMs and ToMs are wall clock: history is a question about days people
	// remember, not about box uptime.
	FromMs int64 `cbor:"fromMs"`
	ToMs   int64 `cbor:"toMs"`
	// Have names tiles the app already caches, so an unchanged tile is never
	// resent. That is the entire transfer model on a metered connection.
	Have []HistTileRef `cbor:"have,omitempty"`
	// MaxPoints caps the answer; the box widens the step to stay under it.
	MaxPoints *int64 `cbor:"maxPoints,omitempty"`
}

// HistTileRef names a cached tile.
type HistTileRef struct {
	TileID string `cbor:"tileId"`
	Etag   string `cbor:"etag"`
}

// HistChunk is one tile of column-packed samples.
type HistChunk struct {
	TileID  string     `cbor:"tileId"`
	Etag    string     `cbor:"etag"`
	Res     Resolution `cbor:"res"`
	StartMs int64      `cbor:"startMs"`
	// StepMs is the point spacing after aggregation. Res says which store the
	// data came from; the two answer different questions.
	StepMs int64 `cbor:"stepMs"`
	// Series names the columns, in Data order.
	Series []string `cbor:"series"`
	// Data is int32 LE, one contiguous block per series. MISSING is
	// math.MinInt32 — distinct from zero, which is a real reading.
	Data []byte `cbor:"data"`
	// Partial marks the trailing tile that is still filling. Never cached.
	Partial bool `cbor:"partial"`
}

// HistEnd closes a query.
type HistEnd struct {
	ResActual Resolution `cbor:"resActual"`
	// Gaps are ranges the box knows it cannot serve, and why. The app says
	// "evicted" or "no data" instead of drawing a hole and guessing.
	Gaps []HistGap `cbor:"gaps"`
}

// HistGap is one such range.
type HistGap struct {
	FromMs int64  `cbor:"fromMs"`
	ToMs   int64  `cbor:"toMs"`
	Reason string `cbor:"reason"`
}

// Gap reasons, as the app spells them.
const (
	GapNoData  = "no_data"
	GapEvicted = "evicted"
	GapBoxDown = "box_down"
)

// --------------------------------------------------------------------------
// The box's own HTTP API, over the session
// --------------------------------------------------------------------------

// APIReq asks the box to run one request against its own HTTP API.
//
// All four api.* messages ride the bulk lane and carry the request id. Never
// lane 0: every field here varies in length with what was asked and answered,
// and lane 0's constant size is a privacy control rather than a budget.
type APIReq struct {
	// Method is one of the six the passthrough accepts. Anything else is
	// refused before a handler runs.
	Method string `cbor:"method"`
	// Path must start "/api/". The box's static handler is unreachable on
	// purpose: serving HTML through the session would be a second origin
	// under another name, which docs/architecture.md rejected.
	Path string `cbor:"path"`
	// Query is parsed, not a raw string. Two reasons: the tier gate keys on
	// the path, and a tier decided over a string that can carry "?" is a
	// parser bug that becomes a privilege bug — and this way the app never
	// handles encoding, because the box rebuilds the URL itself.
	Query map[string]string `cbor:"query,omitempty"`
	// Body is opaque. The box sets Content-Type: application/json when one
	// is present, and sets nothing else.
	Body []byte `cbor:"body,omitempty"`
	// MaxBytes is the app's own ceiling, clamped by the box's APIMaxBytes.
	// The app learns the real figure from APIEnd.
	MaxBytes int64 `cbor:"maxBytes,omitempty"`
	// StepUp says the app ran a passkey ceremony for this request. Read the
	// note above the check in passthrough.go before believing it means more
	// than it does.
	StepUp bool `cbor:"stepUp,omitempty"`

	// There is deliberately no headers field. There is no path by which a
	// client byte becomes a caller claim: identity rides on the request
	// context inside the box process, and nothing about the caller is on the
	// wire. Adding headers later would open exactly that hole.
}

// Methods the passthrough carries.
const (
	APIGet    = "GET"
	APIHead   = "HEAD"
	APIPost   = "POST"
	APIPut    = "PUT"
	APIPatch  = "PATCH"
	APIDelete = "DELETE"
)

// APIHeadMsg is the answer's status line.
//
// Its arrival is what tells the two error kinds apart, and it needs no body
// inspection: if api.head arrived, the box's HTTP layer answered and Status
// is the answer — a 403 or a 500 from a box handler is a status, never an
// E_ code. If an error arrived on that id instead, the passthrough refused
// and no handler ran.
type APIHeadMsg struct {
	Status int `cbor:"status"`
	// Headers is a short allowlist, not the handler's full set. Everything
	// the app needs to read an answer and nothing that describes the box's
	// LAN.
	Headers map[string]string `cbor:"headers"`
	// Len is the declared length, absent when the handler did not declare
	// one — which is the usual case in process. A guessed number here would
	// be a progress bar that lies.
	Len *int64 `cbor:"len"`
}

// APIChunk is one piece of the answer, in order.
type APIChunk struct {
	Seq  uint32 `cbor:"seq"`
	Data []byte `cbor:"data"`
}

// APIEnd closes an answer.
type APIEnd struct {
	Bytes int64 `cbor:"bytes"`
	// Truncated means the answer ran past the ceiling and stops here. The
	// app treats it as a failure rather than showing a partial answer as a
	// whole one.
	Truncated bool `cbor:"truncated"`
}

// --------------------------------------------------------------------------
// Errors, events, teardown
// --------------------------------------------------------------------------

// ErrorBody is a stable code with machine-readable args. No prose: the app
// owns all of it, in every language it ships.
type ErrorBody struct {
	Code         string         `cbor:"code"`
	Retryable    bool           `cbor:"retryable"`
	RetryAfterMs *int64         `cbor:"retryAfterMs,omitempty"`
	Args         map[string]any `cbor:"args,omitempty"`
}

// Event is something worth surfacing.
type Event struct {
	EventID  string         `cbor:"eventId"`
	Kind     string         `cbor:"kind"`
	Severity string         `cbor:"severity"`
	UptimeMs int64          `cbor:"uptimeMs"`
	Args     map[string]any `cbor:"args,omitempty"`
}

// SessionTerminate ends the session from the box's side.
type SessionTerminate struct {
	Reason string `cbor:"reason"`
}

// Termination reasons.
const (
	TerminateRevoked      = "revoked"
	TerminateEpochChanged = "epoch_changed"
	TerminateBoxShutdown  = "box_shutdown"
	TerminateSuperseded   = "superseded"
)

// wireModes projects control's catalogue onto the wire.
//
// The catalogue is the single source of truth for what modes exist and how
// they are presented; hand-writing a list here would let the app offer a mode
// the box does not have, or hide one it does.
func wireModes(cat []control.ModeInfo) []ModeInfo {
	out := make([]ModeInfo, 0, len(cat))
	for _, m := range cat {
		out = append(out, ModeInfo{
			Key:     string(m.Key),
			Label:   m.Label,
			Tooltip: m.Tooltip,
			Tier:    string(m.Tier),
		})
	}
	return out
}
