// Package ftwdbshadow implements the local FTWDB shadow protocol.
//
// The wire codec and client use only the Go standard library. The state mapper
// keeps a narrow local boundary: FTW remains authoritative while it copies data
// to a local FTWDB sidecar.
package ftwdbshadow

import (
	"encoding/hex"
	"fmt"
)

const (
	ProtocolVersion    uint16 = 1
	MaxFrameBytes             = 4 * 1024 * 1024
	MaxBatchPoints            = 16_384
	MaxMetadataRecords        = 16_384
	MaxQueueEntries    uint32 = 65_536
	MaxProperties             = 1_024
	MaxRollupTiers            = 64
	MaxTextBytes              = 4_096
)

var frameMagic = [4]byte{'F', 'T', 'W', 'S'}

// ID128 is a protocol ID in wire order.
type ID128 [16]byte

func ParseID128(value string) (ID128, error) {
	var id ID128
	if len(value) != hex.EncodedLen(len(id)) {
		return id, fmt.Errorf("ftwdb shadow id must contain 32 hex digits")
	}
	if _, err := hex.Decode(id[:], []byte(value)); err != nil {
		return ID128{}, fmt.Errorf("decode ftwdb shadow id: %w", err)
	}
	return id, nil
}

func (id ID128) String() string {
	return hex.EncodeToString(id[:])
}

func (id ID128) IsZero() bool {
	return id == ID128{}
}

type messageKind byte

const (
	kindHelloRequest       messageKind = 1
	kindCommitBatchRequest messageKind = 2
	kindFlushRequest       messageKind = 3
	kindHealthRequest      messageKind = 4
	kindHelloResponse      messageKind = 128
	kindAckResponse        messageKind = 129
	kindHealthResponse     messageKind = 130
	kindErrorResponse      messageKind = 131
)

// Message is one complete v1 request or response.
type Message interface {
	messageKind() messageKind
}

type HelloRequest struct {
	SourceID      ID128
	NodeID        string
	ClientVersion string
	Capabilities  uint64
}

func (HelloRequest) messageKind() messageKind { return kindHelloRequest }

type HelloResponse struct {
	SelectedVersion  uint16
	SessionID        ID128
	ServerTimeMicros int64
}

func (HelloResponse) messageKind() messageKind { return kindHelloResponse }

type CommitBatchRequest struct {
	SourceID  ID128
	Sequence  uint64
	CommitID  ID128
	Entities  []Entity
	Relations []Relation
	Series    []SeriesDefinition
	Runs      []Run
	Plans     []Plan
	Points    []Point
}

func (CommitBatchRequest) messageKind() messageKind { return kindCommitBatchRequest }

type FlushRequest struct {
	SourceID        ID128
	ThroughSequence uint64
}

func (FlushRequest) messageKind() messageKind { return kindFlushRequest }

type HealthRequest struct {
	Nonce uint64
}

func (HealthRequest) messageKind() messageKind { return kindHealthRequest }

type AckKind byte

const (
	AckCommitBatch AckKind = 1
	AckFlush       AckKind = 2
)

type Ack struct {
	Kind                    AckKind
	SourceID                ID128
	Sequence                uint64
	CommitID                ID128
	AcceptedThroughSequence *uint64
	DurableThroughSequence  *uint64
	Durable                 bool
	Deduplicated            bool
	FrameOffset             uint64
	Records                 uint32
	Points                  uint32
	BytesWritten            uint64
}

func (Ack) messageKind() messageKind { return kindAckResponse }

type HealthStatus byte

const (
	HealthHealthy     HealthStatus = 1
	HealthDegraded    HealthStatus = 2
	HealthUnavailable HealthStatus = 3
)

type HealthResponse struct {
	Nonce                   uint64
	SourceID                ID128
	Status                  HealthStatus
	QueueEntries            uint32
	AcceptedThroughSequence *uint64
	DurableThroughSequence  *uint64
	Ops                     *HealthOps
}

type HealthOps struct {
	OverloadCount      uint64 `json:"overload_count"`
	ProtocolErrorCount uint64 `json:"protocol_error_count"`
	DatabaseBytes      uint64 `json:"database_bytes"`
	DatabasePoints     uint64 `json:"database_points"`
	DatabaseCommits    uint64 `json:"database_commits"`
	RecoveredTailBytes uint64 `json:"recovered_tail_bytes"`
	SyncPolicy         byte   `json:"sync_policy"` // 1 always, 2 manual, 3 every N bytes
	SyncEveryBytes     uint64 `json:"sync_every_bytes"`
	LastAckDurable     bool   `json:"last_ack_durable"`
}

func (HealthResponse) messageKind() messageKind { return kindHealthResponse }

type ErrorCode byte

const (
	ErrorInvalidRequest      ErrorCode = 1
	ErrorOverloaded          ErrorCode = 2
	ErrorInternal            ErrorCode = 3
	ErrorUnsupported         ErrorCode = 4
	ErrorIdempotencyConflict ErrorCode = 5
)

type ErrorResponse struct {
	Code      ErrorCode
	Retryable bool
	Message   string
}

func (ErrorResponse) messageKind() messageKind { return kindErrorResponse }

type PropertyKind byte

const (
	PropertyNull    PropertyKind = 0
	PropertyBool    PropertyKind = 1
	PropertyInteger PropertyKind = 2
	PropertyFloat   PropertyKind = 3
	PropertyText    PropertyKind = 4
)

type PropertyValue struct {
	Kind    PropertyKind
	Bool    bool
	Integer int64
	Float   float64
	Text    string
}

func NullProperty() PropertyValue {
	return PropertyValue{Kind: PropertyNull}
}

func BoolProperty(value bool) PropertyValue {
	return PropertyValue{Kind: PropertyBool, Bool: value}
}

func IntegerProperty(value int64) PropertyValue {
	return PropertyValue{Kind: PropertyInteger, Integer: value}
}

func FloatProperty(value float64) PropertyValue {
	return PropertyValue{Kind: PropertyFloat, Float: value}
}

func TextProperty(value string) PropertyValue {
	return PropertyValue{Kind: PropertyText, Text: value}
}

type Entity struct {
	ID         ID128
	Kind       string
	Name       string
	Parent     *ID128
	ValidFrom  int64
	ValidTo    *int64
	Properties map[string]PropertyValue
}

type Relation struct {
	ID         ID128
	Kind       string
	Source     ID128
	Target     ID128
	ValidFrom  int64
	ValidTo    *int64
	Properties map[string]PropertyValue
}

type SeriesSemantics byte

const (
	SeriesGauge         SeriesSemantics = 1
	SeriesIntervalTotal SeriesSemantics = 2
	SeriesCounter       SeriesSemantics = 3
	SeriesState         SeriesSemantics = 4
	SeriesEvent         SeriesSemantics = 5
)

type CalendarUnit byte

const (
	CalendarDay   CalendarUnit = 1
	CalendarMonth CalendarUnit = 2
	CalendarYear  CalendarUnit = 3
)

type RollupResolutionKind byte

const (
	RollupFixedMicros RollupResolutionKind = 1
	RollupCalendar    RollupResolutionKind = 2
)

type RollupResolution struct {
	Kind         RollupResolutionKind
	FixedMicros  int64
	CalendarUnit CalendarUnit
	IANATimezone string
}

type RollupTier struct {
	Resolution      RollupResolution
	RetainForMicros *int64
}

type RollupPolicy struct {
	RawRetainForMicros *int64
	Tiers              []RollupTier
}

type SeriesDefinition struct {
	ID               uint64
	OwnerEntity      *ID128
	OwnerRelation    *ID128
	Name             string
	PhysicalQuantity string
	CanonicalUnit    string
	Semantics        SeriesSemantics
	MaximumGapMicros *int64
	RollupPolicy     RollupPolicy
}

type RunKind byte

const (
	RunForecast       RunKind = 1
	RunOptimization   RunKind = 2
	RunImport         RunKind = 3
	RunControl        RunKind = 4
	RunReconciliation RunKind = 5
)

type RunStatus byte

const (
	RunPending   RunStatus = 1
	RunRunning   RunStatus = 2
	RunSucceeded RunStatus = 3
	RunFailed    RunStatus = 4
	RunCancelled RunStatus = 5
)

type Run struct {
	ID            ID128
	Kind          RunKind
	Status        RunStatus
	CreatedAt     int64
	KnowledgeTime int64
	Workflow      string
	Model         string
	ModelVersion  string
	ParentRun     *ID128
	InputSnapshot *ID128
	Attributes    map[string]PropertyValue
}

type PlanStatus byte

const (
	PlanCandidate  PlanStatus = 1
	PlanApproved   PlanStatus = 2
	PlanDeployed   PlanStatus = 3
	PlanSuperseded PlanStatus = 4
	PlanCancelled  PlanStatus = 5
)

type Plan struct {
	ID               ID128
	RunID            ID128
	Status           PlanStatus
	HorizonStart     int64
	HorizonEnd       int64
	ResolutionMicros int64
	Scenario         string
	ObjectiveTerms   map[string]float64
	ObjectiveValue   *float64
	Supersedes       *ID128
	Attributes       map[string]PropertyValue
}

type Point struct {
	SeriesID      uint64
	ValidTime     int64
	ValidTimeEnd  int64
	KnowledgeTime int64
	ChangeTime    int64
	RunID         ID128
	Value         float64
	Quality       uint32
	Flags         uint32
}
