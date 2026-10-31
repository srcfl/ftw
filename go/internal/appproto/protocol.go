// Package appproto speaks the FTW web app's client protocol.
//
// It owns message semantics only: what a hello_ok says, which lane a reply
// belongs on, when a command may be dispatched, what a plan slot means. The
// names and shapes on the wire live in github.com/srcfl/ftw/go/protocol, which
// FTW Cloud imports as well; this file re-exports them so the box's call sites
// keep their names. The bytes underneath — frame header, bucket padding and
// the Noise session — belong to protocol/appwire, which this package reaches
// through the narrow FrameCodec interface.
//
// Two rules run through everything here and are worth stating once:
//
// Every age is measured against the box's uptime, never its wall clock. A Pi
// has no RTC and reads 1970 until NTP answers, so a wall-clock age would be
// wrong by decades exactly when a user is looking at it after a power cut.
//
// Nothing in this package writes hardware. It validates intent and hands it to
// control, which is where the safety invariants live.
package appproto

import (
	"github.com/fxamacker/cbor/v2"

	"github.com/srcfl/ftw/go/protocol"
)

// The wire format moved to package protocol so that FTW Cloud can import it
// without linking control, the planner or the store. The aliases below are the
// same types, constants and values under their old names. New code may use
// package protocol directly.

// Envelope and framing.
type (
	Envelope   = protocol.Envelope
	Frame      = protocol.Frame
	FrameCodec = protocol.FrameCodec
)

const (
	ProtoMin    = protocol.ProtoMin
	ProtoMax    = protocol.ProtoMax
	ProtoFloor  = protocol.ProtoFloor
	LaneControl = protocol.LaneControl
	LaneBulk    = protocol.LaneBulk
	FlagTrunc   = protocol.FlagTrunc
)

var BulkBuckets = protocol.BulkBuckets

// Marshal encodes a body deterministically.
func Marshal(v any) (cbor.RawMessage, error) { return protocol.Marshal(v) }

// Unmarshal decodes a body. Unknown keys are dropped without complaint.
func Unmarshal(raw cbor.RawMessage, v any) error { return protocol.Unmarshal(raw, v) }

// EncodeEnvelope renders one envelope to CBOR.
func EncodeEnvelope(e Envelope) ([]byte, error) { return protocol.EncodeEnvelope(e) }

// DecodeEnvelope parses one envelope, leaving the body encoded.
func DecodeEnvelope(b []byte) (Envelope, error) { return protocol.DecodeEnvelope(b) }

func newEnvelope(t string, id *uint32, body any) (Envelope, error) {
	return protocol.NewEnvelope(t, id, body)
}

func bulkBucketFor(payloadBytes int) int { return protocol.BulkBucketFor(payloadBytes) }

// Messages.
type (
	BoxMode           = protocol.BoxMode
	Hello             = protocol.Hello
	ProtoRange        = protocol.ProtoRange
	AppInfo           = protocol.AppInfo
	HelloOK           = protocol.HelloOK
	BoxInfo           = protocol.BoxInfo
	Clock             = protocol.Clock
	ModeInfo          = protocol.ModeInfo
	BootProgress      = protocol.BootProgress
	Sub               = protocol.Sub
	SourceWire        = protocol.SourceWire
	FieldDef          = protocol.FieldDef
	Snap              = protocol.Snap
	Delta             = protocol.Delta
	Tick              = protocol.Tick
	Guard             = protocol.Guard
	Cmd               = protocol.Cmd
	Expect            = protocol.Expect
	CmdAck            = protocol.CmdAck
	CmdResult         = protocol.CmdResult
	Observed          = protocol.Observed
	PlanReason        = protocol.PlanReason
	PlanSlot          = protocol.PlanSlot
	Plan              = protocol.Plan
	PlanCharger       = protocol.PlanCharger
	PlanChargerWindow = protocol.PlanChargerWindow
	PriceGet          = protocol.PriceGet
	PriceSlot         = protocol.PriceSlot
	Price             = protocol.Price
	HistQuery         = protocol.HistQuery
	HistTileRef       = protocol.HistTileRef
	HistChunk         = protocol.HistChunk
	HistEnd           = protocol.HistEnd
	HistGap           = protocol.HistGap
	APIReq            = protocol.APIReq
	APIHeadMsg        = protocol.APIHeadMsg
	APIChunk          = protocol.APIChunk
	APIEnd            = protocol.APIEnd
	ErrorBody         = protocol.ErrorBody
	Event             = protocol.Event
	SessionTerminate  = protocol.SessionTerminate
	CloudSession      = protocol.CloudSession
	ExchangeHelloOK   = protocol.ExchangeHelloOK
	Now               = protocol.Now
	NowSource         = protocol.NowSource
	NowLoadpoint      = protocol.NowLoadpoint
)

const (
	MsgHello                  = protocol.MsgHello
	MsgHelloOK                = protocol.MsgHelloOK
	MsgSub                    = protocol.MsgSub
	MsgSnap                   = protocol.MsgSnap
	MsgDelta                  = protocol.MsgDelta
	MsgTick                   = protocol.MsgTick
	MsgPlanGet                = protocol.MsgPlanGet
	MsgPlan                   = protocol.MsgPlan
	MsgCmd                    = protocol.MsgCmd
	MsgCmdAck                 = protocol.MsgCmdAck
	MsgCmdResult              = protocol.MsgCmdResult
	MsgEvent                  = protocol.MsgEvent
	MsgError                  = protocol.MsgError
	MsgSessionTerminate       = protocol.MsgSessionTerminate
	MsgPriceGet               = protocol.MsgPriceGet
	MsgPrice                  = protocol.MsgPrice
	MsgHistQuery              = protocol.MsgHistQuery
	MsgHistChunk              = protocol.MsgHistChunk
	MsgHistEnd                = protocol.MsgHistEnd
	MsgAPIReq                 = protocol.MsgAPIReq
	MsgAPIHead                = protocol.MsgAPIHead
	MsgAPIChunk               = protocol.MsgAPIChunk
	MsgAPIEnd                 = protocol.MsgAPIEnd
	MsgNow                    = protocol.MsgNow
	OpSetMode                 = protocol.OpSetMode
	OpPlannerPrefsSet         = protocol.OpPlannerPrefsSet
	OpSiteTimeZonePropose     = protocol.OpSiteTimeZonePropose
	OpLoadpointHold           = protocol.OpLoadpointHold
	OpLoadpointBoost          = protocol.OpLoadpointBoost
	OpLoadpointSoCSet         = protocol.OpLoadpointSoCSet
	OpLoadpointSurplusOnlySet = protocol.OpLoadpointSurplusOnlySet
	OpCarGoalSet              = protocol.OpCarGoalSet
	OpCarSoCSet               = protocol.OpCarSoCSet
	OpSessionCarSet           = protocol.OpSessionCarSet
	OpCarAdd                  = protocol.OpCarAdd
	OpCarUpdate               = protocol.OpCarUpdate
	OpCarRemove               = protocol.OpCarRemove
	BoxModeFull               = protocol.BoxModeFull
	BoxModeFloor              = protocol.BoxModeFloor
	BoxModeBooting            = protocol.BoxModeBooting
	BoxModeReadonly           = protocol.BoxModeReadonly
	HintAppUpdate             = protocol.HintAppUpdate
	BootPhaseVacuum           = protocol.BootPhaseVacuum
	BootPhaseMigrate          = protocol.BootPhaseMigrate
	BootPhaseDrivers          = protocol.BootPhaseDrivers
	GuardLT                   = protocol.GuardLT
	GuardLTE                  = protocol.GuardLTE
	GuardGT                   = protocol.GuardGT
	GuardGTE                  = protocol.GuardGTE
	CmdApplied                = protocol.CmdApplied
	CmdRejected               = protocol.CmdRejected
	CmdExpired                = protocol.CmdExpired
	CmdSuperseded             = protocol.CmdSuperseded
	CmdUnconfirmed            = protocol.CmdUnconfirmed
	ObservedSrcCore           = protocol.ObservedSrcCore
	ReasonCheapImport         = protocol.ReasonCheapImport
	ReasonExpensiveImport     = protocol.ReasonExpensiveImport
	ReasonSolarSurplus        = protocol.ReasonSolarSurplus
	ReasonPeakShaving         = protocol.ReasonPeakShaving
	ReasonReserveHeld         = protocol.ReasonReserveHeld
	ReasonExportPaid          = protocol.ReasonExportPaid
	ReasonIdle                = protocol.ReasonIdle
	GapNoData                 = protocol.GapNoData
	GapEvicted                = protocol.GapEvicted
	GapBoxDown                = protocol.GapBoxDown
	APIGet                    = protocol.APIGet
	APIHead                   = protocol.APIHead
	APIPost                   = protocol.APIPost
	APIPut                    = protocol.APIPut
	APIPatch                  = protocol.APIPatch
	APIDelete                 = protocol.APIDelete
	TerminateRevoked          = protocol.TerminateRevoked
	TerminateEpochChanged     = protocol.TerminateEpochChanged
	TerminateBoxShutdown      = protocol.TerminateBoxShutdown
	TerminateSuperseded       = protocol.TerminateSuperseded
)

// Names generated from contract/registry.yaml.
type (
	Fid         = protocol.Fid
	SourceState = protocol.SourceState
	Resolution  = protocol.Resolution
)

const (
	ContractVersion       = protocol.ContractVersion
	FidMode               = protocol.FidMode
	FidGridW              = protocol.FidGridW
	FidPvW                = protocol.FidPvW
	FidBatteryW           = protocol.FidBatteryW
	FidBatterySoc         = protocol.FidBatterySoc
	FidLoadW              = protocol.FidLoadW
	FidSrcGrid            = protocol.FidSrcGrid
	FidSrcPv              = protocol.FidSrcPv
	FidSrcBattery         = protocol.FidSrcBattery
	FidEvW                = protocol.FidEvW
	FidGridL1Da           = protocol.FidGridL1Da
	FidGridL2Da           = protocol.FidGridL2Da
	FidGridL3Da           = protocol.FidGridL3Da
	FidGridFuseDa         = protocol.FidGridFuseDa
	CapStatusCore         = protocol.CapStatusCore
	CapStatusPhases       = protocol.CapStatusPhases
	CapStatusDrivers      = protocol.CapStatusDrivers
	CapHistory5m          = protocol.CapHistory5m
	CapHistory1h          = protocol.CapHistory1h
	CapHistoryEtag        = protocol.CapHistoryEtag
	CapCmdLease           = protocol.CapCmdLease
	CapCmdPrecondition    = protocol.CapCmdPrecondition
	CapCmdReadback        = protocol.CapCmdReadback
	CapDerBattery         = protocol.CapDerBattery
	CapDerEv              = protocol.CapDerEv
	CapDerV2x             = protocol.CapDerV2x
	CapPlanDispatch       = protocol.CapPlanDispatch
	CapNetWebrtc          = protocol.CapNetWebrtc
	CapPriceSpot          = protocol.CapPriceSpot
	CapApiPassthrough     = protocol.CapApiPassthrough
	CapCloudLink          = protocol.CapCloudLink
	CapCloudTun           = protocol.CapCloudTun
	CapCmdExchange        = protocol.CapCmdExchange
	CapAccessAgents       = protocol.CapAccessAgents
	CapAccessInvites      = protocol.CapAccessInvites
	ScopeLiveRead         = protocol.ScopeLiveRead
	ScopeHistoryRead      = protocol.ScopeHistoryRead
	ScopePlanRead         = protocol.ScopePlanRead
	ScopeHealthRead       = protocol.ScopeHealthRead
	ScopeAssetsRead       = protocol.ScopeAssetsRead
	ScopeDispatchWrite    = protocol.ScopeDispatchWrite
	ScopeModeWrite        = protocol.ScopeModeWrite
	ScopeMembersRead      = protocol.ScopeMembersRead
	ScopeMembersWrite     = protocol.ScopeMembersWrite
	ScopeCarsWrite        = protocol.ScopeCarsWrite
	ScopeSetupWrite       = protocol.ScopeSetupWrite
	CapDerCar             = protocol.CapDerCar
	ErrBooting            = protocol.ErrBooting
	ErrUnknownOp          = protocol.ErrUnknownOp
	ErrCmdExpired         = protocol.ErrCmdExpired
	ErrPrecondition       = protocol.ErrPrecondition
	ErrConflict           = protocol.ErrConflict
	ErrScopeDenied        = protocol.ErrScopeDenied
	ErrGrantRevoked       = protocol.ErrGrantRevoked
	ErrLastOwnerProtected = protocol.ErrLastOwnerProtected
	ErrRangeTooLarge      = protocol.ErrRangeTooLarge
	ErrUnavailable        = protocol.ErrUnavailable
	ErrNeedsStepUp        = protocol.ErrNeedsStepUp
	ErrUseCmd             = protocol.ErrUseCmd
	ErrUnsupportedMedia   = protocol.ErrUnsupportedMedia
	ErrWholeDocument      = protocol.ErrWholeDocument
	ErrLocalOnly          = protocol.ErrLocalOnly
	ErrNotFound           = protocol.ErrNotFound
	ErrDuplicate          = protocol.ErrDuplicate
	SourceLive            = protocol.SourceLive
	SourceLagging         = protocol.SourceLagging
	SourceStale           = protocol.SourceStale
	SourceDown            = protocol.SourceDown
	SourceNever           = protocol.SourceNever
	ResN1h                = protocol.ResN1h
	ResN5m                = protocol.ResN5m
)

var (
	FrozenFieldNames        = protocol.FrozenFieldNames
	FrozenFieldUnits        = protocol.FrozenFieldUnits
	FieldNames              = protocol.FieldNames
	FieldUnits              = protocol.FieldUnits
	AllCapabilities         = protocol.AllCapabilities
	WriteScopes             = protocol.WriteScopes
	RegistryOps             = protocol.RegistryOps
	ErrorRetryable          = protocol.ErrorRetryable
	ResolutionTileSpanMs    = protocol.ResolutionTileSpanMs
	ResolutionRetentionDays = protocol.ResolutionRetentionDays
	RegistryModeTiers       = protocol.RegistryModeTiers
)

// History tiles.
type HistorySample = protocol.HistorySample
