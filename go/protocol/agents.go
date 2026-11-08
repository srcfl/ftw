package protocol

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// strictDec decodes a signed body: a key it does not know, or a key twice,
// is a body the box will not sign off on.
var strictDec, _ = cbor.DecOptions{
	DupMapKey:         cbor.DupMapKeyEnforcedAPF,
	ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
}.DecMode()

// Agents: grants an owner gives to an assistant, to Sourceful support for one
// ticket, or to the release check. docs/agents.md is the normative text; this
// file holds the shapes and the approval signature's bytes, so FTW Cloud and
// the phone build them from the same code the box checks them with.

// Agent message types. Each request gets MsgAgentOK or MsgError with its id.
const (
	MsgApprovalKeySet = "approval.key.set"
	MsgAgentAdd       = "agent.add"
	MsgAgentWiden     = "agent.widen"
	MsgAgentRevoke    = "agent.revoke"
	MsgAgentList      = "agent.list"
	MsgAgentRequest   = "agent.request"
	MsgAgentConfirm   = "agent.confirm"
	MsgAgentOK        = "agent.ok"
	// MsgAgentDecided tells an agent's open session how its request ended.
	MsgAgentDecided = "agent.decided"
	MsgAuditGet     = "audit.get"

	MsgHealthGet = "health.get"
	MsgHealth    = "health"
	MsgEventsGet = "events.get"
	MsgEvents    = "events"
	MsgLogsGet   = "logs.get"
	MsgLogs      = "logs"
	MsgConfigGet = "config.get"
	MsgConfig    = "config"
	MsgDiagRun   = "diag.run"
	MsgDiag      = "diag"

	// The typed reads, whose answers name nothing. live, loadpoints and
	// cars carry FTW Cloud's class bodies (cloudwire.Live, LP and Cars)
	// with the box's agent ids in place of the copy's.
	MsgLiveGet       = "live.get"
	MsgLive          = "live"
	MsgLoadpointsGet = "loadpoints.get"
	MsgLoadpoints    = "loadpoints"
	MsgCarsGet       = "cars.get"
	MsgCars          = "cars"
	MsgDriversGet    = "drivers.get"
	MsgDrivers       = "drivers"
)

// TerminateExpired ends an agent's session when its grant runs out.
const TerminateExpired = "expired"

// Request states.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDeclined = "declined"
	RequestExpired  = "expired"
)

// Limits the box enforces on every grant, whatever the grant says.
const (
	// AgentMaxMs, SupportMaxMs and ReleaseMaxMs are the longest grants.
	// The box tells owners its own in agent.list, as LongestMs.
	AgentMaxMs   int64 = 365 * 24 * 60 * 60 * 1000
	SupportMaxMs int64 = 14 * 24 * 60 * 60 * 1000
	ReleaseMaxMs int64 = 90 * 24 * 60 * 60 * 1000

	// AgentRowsMax is how many agent rows one box holds.
	AgentRowsMax = 16

	HoldSMin  = 60
	HoldSMax  = 1800
	MaxWMin   = 1
	MaxWMax   = 100_000
	PerMinMin = 1
	PerMinMax = 60
	// PerMinDefault is the rate a grant that names none gets.
	PerMinDefault = 6
	// ReadsPerMin is the box's own limit on an agent's reads.
	ReadsPerMin = 60

	// PendingPerAgent and PendingPerBox bound the requests waiting for an
	// owner. A request lives RequestTTLMs and is kept RequestKeptMs after
	// it ends, so the agent can read how it ended.
	PendingPerAgent       = 4
	PendingPerBox         = 16
	RequestTTLMs    int64 = 12 * 60 * 60 * 1000
	RequestKeptMs   int64 = 24 * 60 * 60 * 1000
	RequestArgsMax        = 1024
	RequestWhyMax         = 280

	// ApprovalSkewMs is how far an approval's atMs may stray from the box's
	// wall clock, and ApprovalReplayMs how long the box remembers one it took.
	ApprovalSkewMs   int64 = 10 * 60 * 1000
	ApprovalReplayMs int64 = 20 * 60 * 1000

	// LogsLinesMax bounds logs.get; the answer also fits one bulk frame.
	LogsLinesMax  = 200
	EventsMax     = 50
	AuditPageMax  = 200
	AuditRowsKept = 10_000
	AuditDaysKept = 90
	// ByMax bounds the staff id a support call carries.
	ByMax = 64
	// ReqIDBytes is a request id's randomness: 128 bits.
	ReqIDBytes = 16
	// OfferTTLMs is the longest a signed offer may live.
	OfferTTLMs int64 = 10 * 60 * 1000
	// PrincipalFieldMax bounds each principal field.
	PrincipalFieldMax = 256
)

// Caps bound what one grant may do.
type Caps struct {
	// HoldS is the longest hold, in seconds.
	HoldS uint64 `cbor:"holdS" json:"holdS"`
	// MaxW is the largest power a hold may set, in W.
	MaxW uint64 `cbor:"maxW" json:"maxW"`
	// PerMin is cmd and agent.request per rolling minute.
	PerMin uint64 `cbor:"perMin" json:"perMin"`
}

// Principal says who an agent row is for. Display and audit only: the role
// decides what it may do.
type Principal struct {
	Client   string `cbor:"client" json:"client"`
	ClientID string `cbor:"clientId" json:"clientId"`
	Ticket   string `cbor:"ticket" json:"ticket"`
}

// AgentRow is one agent grant as the box holds it.
type AgentRow struct {
	ID          string    `cbor:"id" json:"id"`
	Key         []byte    `cbor:"key" json:"key"`
	Role        string    `cbor:"role" json:"role"`
	Scopes      []string  `cbor:"scopes" json:"scopes"`
	Ops         []string  `cbor:"ops" json:"ops"`
	Caps        Caps      `cbor:"caps" json:"caps"`
	ExpiresAtMs int64     `cbor:"expiresAtMs" json:"expiresAtMs"`
	Principal   Principal `cbor:"principal" json:"principal"`
	ApprovedBy  string    `cbor:"approvedBy" json:"approvedBy"`
	AddedAtMs   int64     `cbor:"addedAtMs" json:"addedAtMs"`
	LastSeenMs  int64     `cbor:"lastSeenMs" json:"lastSeenMs"`
	Epoch       uint64    `cbor:"epoch" json:"epoch"`
}

// ApprovalKeySet registers an owner phone's approval key.
type ApprovalKeySet struct {
	Pub  []byte `cbor:"pub"`
	AtMs int64  `cbor:"atMs"`
	Sig  []byte `cbor:"sig"`
	// PrevSig is the phone's current key's signature over the same bytes.
	// Replacing a key needs it.
	PrevSig []byte `cbor:"prevSig,omitempty"`
}

// AgentAdd grants access.
type AgentAdd struct {
	Key         []byte    `cbor:"key"`
	Role        string    `cbor:"role"`
	Scopes      []string  `cbor:"scopes"`
	Ops         []string  `cbor:"ops"`
	Caps        Caps      `cbor:"caps"`
	ExpiresAtMs int64     `cbor:"expiresAtMs"`
	Principal   Principal `cbor:"principal"`
	AtMs        int64     `cbor:"atMs"`
	Sig         []byte    `cbor:"sig"`
}

// AgentWiden replaces a grant's rights. Any change but a revoke is signed.
type AgentWiden struct {
	ID          string   `cbor:"id"`
	Epoch       uint64   `cbor:"epoch"`
	Scopes      []string `cbor:"scopes"`
	Ops         []string `cbor:"ops"`
	Caps        Caps     `cbor:"caps"`
	ExpiresAtMs int64    `cbor:"expiresAtMs"`
	AtMs        int64    `cbor:"atMs"`
	Sig         []byte   `cbor:"sig"`
}

// GrantOffer is what FTW Cloud's consent page offers the owner: the agent's
// key and the rights its client asks for (docs/agents.md, The grant offer).
// It travels signed, as SignedOffer. The owner may grant less than it asks.
type GrantOffer struct {
	V uint64 `cbor:"v"`
	// Home is empty: the owner picks the home in the app or on the box page.
	// A box takes an offer whose home is empty or its own.
	Home string `cbor:"home"`
	Key  []byte `cbor:"key"`
	Role string `cbor:"role"`
	// Client is the name the client gives itself. ClientHost is the host of
	// its metadata document or redirect URI, and Verified says FTW Cloud
	// knows the client by its own list. The app shows all three.
	Client     string   `cbor:"client"`
	ClientID   string   `cbor:"clientId"`
	ClientHost string   `cbor:"clientHost"`
	Verified   bool     `cbor:"verified"`
	Scopes     []string `cbor:"scopes"`
	Ops        []string `cbor:"ops"`
	Caps       Caps     `cbor:"caps"`
	TTLS       uint64   `cbor:"ttlS"`
	Ticket     string   `cbor:"ticket"`
	// Code is what the consent page shows, for the owner to type or compare.
	Code string `cbor:"code"`
	// ExpMs is when the offer stops being good, wall clock, at most
	// OfferTTLMs after FTW Cloud made it.
	ExpMs int64 `cbor:"exp"`
}

// SignedOffer is an offer as it travels: O is the offer's deterministic
// CBOR, Sig FTW Cloud's offer key's signature over OfferBytes(O), and Kid
// names that key. It goes base64url, without padding, in a link's fragment.
type SignedOffer struct {
	O   []byte `cbor:"o"`
	Kid string `cbor:"kid"`
	Sig []byte `cbor:"sig"`
}

// OfferTag starts what an offer key signs.
const OfferTag = "ftw.offer.v1"

// SupportClientID is the one client a support offer may come from: the
// support tooling's client at FTW Cloud.
const SupportClientID = "https://cloud.ftw.energy/clients/sourceful-support"

// Errors from reading a signed offer.
var (
	ErrOfferSignature = errors.New("protocol: the offer is not signed by a pinned offer key")
	ErrOfferExpired   = errors.New("protocol: the offer has expired")
	ErrOfferShape     = errors.New("protocol: the offer is malformed")
)

// UnmarshalSigned decodes a body the box checks a signature on: a key it
// does not know, or a key twice, is refused.
func UnmarshalSigned(raw []byte, v any) error {
	if err := strictDec.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("protocol: decode signed body: %w", err)
	}
	return nil
}

// OfferKid names an offer key: the first 16 characters of base64url, without
// padding, of SHA-256 over its SEC1 point. FTW Cloud publishes kid and key at
// /.well-known/ftw-offer-key; the box and the apps pin a list of them.
func OfferKid(pub []byte) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

// OfferBytes is what an offer key signs: ["ftw.offer.v1", o].
func OfferBytes(o []byte) ([]byte, error) {
	return encMode.Marshal([]any{OfferTag, o})
}

// OpenOffer checks a signed offer against pinned offer keys (kid → SEC1
// P-256 point) and the wall clock, and returns the offer. A support offer
// must come from SupportClientID.
func OpenOffer(keys map[string][]byte, so SignedOffer, nowMs int64) (GrantOffer, error) {
	pub, ok := keys[so.Kid]
	if !ok {
		return GrantOffer{}, ErrOfferSignature
	}
	msg, err := OfferBytes(so.O)
	if err != nil {
		return GrantOffer{}, ErrOfferShape
	}
	if VerifyApproval(pub, msg, so.Sig) != nil {
		return GrantOffer{}, ErrOfferSignature
	}
	var o GrantOffer
	if err := strictDec.Unmarshal(so.O, &o); err != nil || o.V != GrantOfferVersion || len(o.Key) != 32 {
		return GrantOffer{}, ErrOfferShape
	}
	if nowMs >= o.ExpMs || o.ExpMs > nowMs+OfferTTLMs {
		return GrantOffer{}, ErrOfferExpired
	}
	if o.Role == RoleSupport && o.ClientID != SupportClientID {
		return GrantOffer{}, ErrOfferShape
	}
	return o, nil
}

// GrantOfferVersion is GrantOffer.V.
const GrantOfferVersion = 1

// AgentRevoke removes a grant. No signature.
type AgentRevoke struct {
	ID string `cbor:"id"`
}

// AgentArg is one argument an agent may put in an op, from the registry's
// agent_args. Min and Max bound a number; Cap names the grant's cap that
// bounds it as well.
type AgentArg struct {
	Name     string
	Kind     string
	Unit     string
	Min      *float64
	Max      *float64
	Values   []string
	Required bool
	Cap      string
	Label    string
}

func bound(v float64) *float64 { return &v }

// AgentRequest is an agent asking for a durable change.
type AgentRequest struct {
	Op   string         `cbor:"op"`
	Args map[string]any `cbor:"args"`
	Why  string         `cbor:"why"`
}

// AgentConfirm answers a request. Approving is signed; declining is not.
type AgentConfirm struct {
	ReqID   string `cbor:"reqId"`
	Approve bool   `cbor:"approve"`
	AtMs    int64  `cbor:"atMs,omitempty"`
	Sig     []byte `cbor:"sig,omitempty"`
}

// Request is one durable change an agent asked for.
type Request struct {
	ReqID       string         `cbor:"reqId" json:"reqId"`
	Agent       string         `cbor:"agent" json:"agent"`
	Op          string         `cbor:"op" json:"op"`
	Args        map[string]any `cbor:"args" json:"args"`
	Why         string         `cbor:"why" json:"why"`
	AtMs        int64          `cbor:"atMs" json:"atMs"`
	ExpiresAtMs int64          `cbor:"expiresAtMs" json:"expiresAtMs"`
	State       string         `cbor:"state" json:"state"`
}

// ApprovalKeyInfo is one owner phone's registered approval key.
type ApprovalKeyInfo struct {
	DeviceID string `cbor:"deviceId" json:"deviceId"`
	SetAtMs  int64  `cbor:"setAtMs" json:"setAtMs"`
}

// AgentDecided is how a request ended, sent to the agent's open session.
// Result is the command's outcome when the request ran.
type AgentDecided struct {
	ReqID  string     `cbor:"reqId"`
	State  string     `cbor:"state"`
	CmdID  string     `cbor:"cmdId,omitempty"`
	Result *CmdResult `cbor:"result,omitempty"`
}

// AgentList is agent.ok's body for agent.list. An agent sees its own row.
type AgentList struct {
	Agents   []AgentRow        `cbor:"agents" json:"agents"`
	Requests []Request         `cbor:"requests" json:"requests"`
	Keys     []ApprovalKeyInfo `cbor:"keys" json:"keys"`
	// LongestMs is the longest grant the box takes for each role, so a
	// client offers no length the box would refuse. Owners only.
	LongestMs map[string]int64 `cbor:"longestMs,omitempty" json:"longestMs,omitempty"`
}

// LongestMs is the longest grant for each agent role, as agent.list tells
// owners. A box whose answer leaves it out takes at most 30 days for an
// agent (docs/agents.md, Mixed versions).
func LongestMs() map[string]int64 {
	return map[string]int64{RoleAgent: AgentMaxMs, RoleSupport: SupportMaxMs, RoleRelease: ReleaseMaxMs}
}

// AuditGet pages through the audit log, newest first.
type AuditGet struct {
	BeforeSeq uint64 `cbor:"beforeSeq,omitempty"`
	N         uint64 `cbor:"n,omitempty"`
}

// AuditRow is one line of the box's audit log.
type AuditRow struct {
	Seq       uint64 `cbor:"seq" json:"seq"`
	AtMs      int64  `cbor:"atMs" json:"atMs"`
	Actor     string `cbor:"actor" json:"actor"`
	Role      string `cbor:"role" json:"role"`
	Principal string `cbor:"principal" json:"principal"`
	By        string `cbor:"by" json:"by"`
	What      string `cbor:"what" json:"what"`
	Target    string `cbor:"target" json:"target"`
	Result    string `cbor:"result" json:"result"`
	// Count folds an agent's reads: how many of what in that minute.
	Count uint64 `cbor:"count" json:"count"`
}

// AuditPage is agent.ok's body for audit.get.
type AuditPage struct {
	Rows []AuditRow `cbor:"rows" json:"rows"`
	More bool       `cbor:"more" json:"more"`
}

// LogsGet asks for a redacted log excerpt: lines older than BeforeMs, or
// newer than AfterMs, newest first up to N.
type LogsGet struct {
	N        uint64 `cbor:"n,omitempty"`
	Level    string `cbor:"level,omitempty"`
	BeforeMs int64  `cbor:"beforeMs,omitempty"`
	AfterMs  int64  `cbor:"afterMs,omitempty"`
}

// EventsGet asks what the box recorded about control between AfterMs and
// BeforeMs, the last 24 hours when both are 0: each stretch in which a
// device function, a source, the site's dispatch or the mode held one
// verdict, each scored plan slot, and the notifications the box sent. Src
// keeps one source by its agent id; MinSeverity "warning" keeps warnings and
// alarms, "alarm" alarms only. N bounds the spans, newest first.
type EventsGet struct {
	N           uint64 `cbor:"n,omitempty"`
	BeforeMs    int64  `cbor:"beforeMs,omitempty"`
	AfterMs     int64  `cbor:"afterMs,omitempty"`
	Src         string `cbor:"src,omitempty"`
	MinSeverity string `cbor:"minSeverity,omitempty"`
}

// EventSpan is one stretch in which a subject held one control verdict.
// Kind is battery, ev, pv, v2x_charger or device for a controlled function,
// source for a driver's freshness, dispatch for whether the site may act,
// mode for the operating mode, or slot for a scored plan slot. Status,
// reason and severity are Core's own words, as in control_feedback.
type EventSpan struct {
	Kind     string `cbor:"kind"`
	Src      string `cbor:"src,omitempty"`
	Status   string `cbor:"status"`
	Reason   string `cbor:"reason,omitempty"`
	Severity string `cbor:"severity,omitempty"`
	Mode     string `cbor:"mode,omitempty"`
	StartMs  int64  `cbor:"startMs"`
	// EndMs is 0 while the verdict still holds.
	EndMs   int64 `cbor:"endMs"`
	Samples int64 `cbor:"samples"`
	// RequestedW and ActualW summarise the power Core asked for and the
	// power measured over the span; absent when never known.
	RequestedW *PowerRange `cbor:"requestedW,omitempty"`
	ActualW    *PowerRange `cbor:"actualW,omitempty"`
	// Last holds the numbers of the latest sample by field name, such as
	// sent_w, readback_w, offered_a, battery_soc or planned_wh.
	Last map[string]float64 `cbor:"last,omitempty"`
}

// PowerRange is the least, most and mean watts over a span.
type PowerRange struct {
	Min  float64 `cbor:"min"`
	Max  float64 `cbor:"max"`
	Mean float64 `cbor:"mean"`
}

// EventNotice is one notification the box sent or tried to send.
type EventNotice struct {
	Ts       int64  `cbor:"ts"`
	Type     string `cbor:"type"`
	Src      string `cbor:"src,omitempty"`
	Status   string `cbor:"status"`
	Priority int    `cbor:"priority,omitempty"`
}

// Events is the answer to events.get: spans newest first, the notices in
// the same window, and More when either was cut. Events is kept for
// readers built before spans; the box leaves it empty.
type Events struct {
	Events  []cbor.RawMessage `cbor:"events"`
	Spans   []EventSpan       `cbor:"spans,omitempty"`
	Notices []EventNotice     `cbor:"notices,omitempty"`
	More    bool              `cbor:"more"`
}

// DriverState is one driver in drivers.get, by its opaque source id.
type DriverState struct {
	Src   string `cbor:"src"`
	Kind  string `cbor:"kind"`
	State string `cbor:"state"`
	// AgeMs is the time since its last good reading, or -1 for never.
	AgeMs int64 `cbor:"ageMs"`
}

// Drivers is the answer to drivers.get.
type Drivers struct {
	Drivers []DriverState `cbor:"drivers"`
}

// LogLine is one redacted line. Src is an opaque source id for a driver's
// line, empty for the box's own.
type LogLine struct {
	Ts    int64  `cbor:"ts"`
	Level string `cbor:"level"`
	Src   string `cbor:"src,omitempty"`
	Msg   string `cbor:"msg"`
}

// Logs is the answer to logs.get, oldest first. Dropped counts lines left
// out to fit one frame; More says older lines remain.
type Logs struct {
	Lines   []LogLine `cbor:"lines"`
	Dropped uint64    `cbor:"dropped"`
	More    bool      `cbor:"more"`
}

// ValidBy says whether a support call's by is well formed: staff:<id> for a
// person or bot:<name> for Sourceful's own tooling.
func ValidBy(by string) bool {
	kind, id, ok := strings.Cut(by, ":")
	if !ok || (kind != "staff" && kind != "bot") || id == "" || len(by) > ByMax {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._@-", c)) {
			return false
		}
	}
	return true
}

// --------------------------------------------------------------------------
// The approval signature
// --------------------------------------------------------------------------

// ApprovalTag starts every signed array, so a signature for anything else is
// never one of these.
const ApprovalTag = "ftw.approval.v1"

// ApprovalSigBytes is r ‖ s, 32 bytes each.
const ApprovalSigBytes = 64

// ApprovalPubBytes is a SEC1 uncompressed P-256 point.
const ApprovalPubBytes = 65

// Errors from checking a signed body's shape. A signature that does not
// verify is ErrApprovalSignature.
var (
	ErrApprovalUnsorted  = errors.New("protocol: a signed list must be sorted with no repeats")
	ErrApprovalPub       = errors.New("protocol: approval key is not an uncompressed P-256 point")
	ErrApprovalSignature = errors.New("protocol: approval signature does not verify")
)

// SignedFor names the box and the phone an approval belongs to.
type SignedFor struct {
	// BoxKey is the box's X25519 static key, as in the pairing QR code.
	BoxKey []byte
	// DeviceID is the signing phone's device id.
	DeviceID string
}

// ApprovalBytes is the deterministic CBOR array a phone signs: the tag, what
// it approves, the box, the phone, its clock, then the fields.
func ApprovalBytes(what string, f SignedFor, atMs int64, fields ...any) ([]byte, error) {
	for _, v := range fields {
		if list, ok := v.([]string); ok && !sortedUnique(list) {
			return nil, ErrApprovalUnsorted
		}
	}
	arr := append([]any{ApprovalTag, what, f.BoxKey, f.DeviceID, atMs}, fields...)
	b, err := encMode.Marshal(arr)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode approval: %w", err)
	}
	return b, nil
}

func capsArray(c Caps) []uint64 { return []uint64{c.HoldS, c.MaxW, c.PerMin} }

func emptyIfNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// KeySetBytes is what approval.key.set signs: the key itself, as proof the
// phone holds its private half.
func KeySetBytes(f SignedFor, m ApprovalKeySet) ([]byte, error) {
	return ApprovalBytes(MsgApprovalKeySet, f, m.AtMs, m.Pub)
}

// AddBytes is what agent.add signs.
func AddBytes(f SignedFor, m AgentAdd) ([]byte, error) {
	return ApprovalBytes(MsgAgentAdd, f, m.AtMs, m.Key, m.Role, emptyIfNil(m.Scopes), emptyIfNil(m.Ops),
		capsArray(m.Caps), m.ExpiresAtMs, m.Principal.Client, m.Principal.ClientID, m.Principal.Ticket)
}

// WidenBytes is what agent.widen signs. key is the row's whole key, which
// the phone read from agent.list: an id and an epoch alone could name
// another row that took the same id.
func WidenBytes(f SignedFor, m AgentWiden, key []byte) ([]byte, error) {
	return ApprovalBytes(MsgAgentWiden, f, m.AtMs, m.ID, key, m.Epoch, emptyIfNil(m.Scopes), emptyIfNil(m.Ops),
		capsArray(m.Caps), m.ExpiresAtMs)
}

// ConfirmBytes is what agent.confirm signs to approve: the request, the
// agent that asked, the op and the hash of its args, as the phone showed
// them.
func ConfirmBytes(f SignedFor, m AgentConfirm, req Request) ([]byte, error) {
	sum, err := ArgsHash(req.Args)
	if err != nil {
		return nil, err
	}
	return ApprovalBytes(MsgAgentConfirm, f, m.AtMs, m.ReqID, req.Agent, req.Op, sum)
}

// ArgsHash is SHA-256 over the deterministic CBOR of a request's args.
func ArgsHash(args map[string]any) ([]byte, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := encMode.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode args: %w", err)
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

// ParseApprovalKey reads a SEC1 uncompressed P-256 public key.
func ParseApprovalKey(pub []byte) (*ecdsa.PublicKey, error) {
	if len(pub) != ApprovalPubBytes || pub[0] != 4 {
		return nil, ErrApprovalPub
	}
	x := new(big.Int).SetBytes(pub[1:33])
	y := new(big.Int).SetBytes(pub[33:])
	curve := elliptic.P256()
	if !curve.IsOnCurve(x, y) {
		return nil, ErrApprovalPub
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

// VerifyApproval checks sig, r ‖ s, over SHA-256 of msg.
func VerifyApproval(pub []byte, msg, sig []byte) error {
	key, err := ParseApprovalKey(pub)
	if err != nil {
		return err
	}
	if len(sig) != ApprovalSigBytes {
		return ErrApprovalSignature
	}
	digest := sha256.Sum256(msg)
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(key, digest[:], r, s) {
		return ErrApprovalSignature
	}
	return nil
}

func sortedUnique(list []string) bool {
	return slices.IsSorted(list) && len(slices.Compact(slices.Clone(list))) == len(list)
}

// Refusal is a refusal on its way to the wire: a registry code and its
// machine-readable args. The box's layers return it so the one that answers
// the caller need not translate.
type Refusal struct {
	Code string
	Args map[string]any
}

func (r *Refusal) Error() string { return fmt.Sprintf("protocol: refused %s %v", r.Code, r.Args) }

// Refuse builds a Refusal.
func Refuse(code string, args map[string]any) *Refusal { return &Refusal{Code: code, Args: args} }
