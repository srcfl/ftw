package ftwdbshadow

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	headerBytes       = 12
	checksumBytes     = 4
	maxPayloadBytes   = MaxFrameBytes - headerBytes - checksumBytes
	maxKeyBytes       = 256
	maxErrorTextBytes = 512
	pointBytes        = 72
)

type ProtocolErrorKind string

const (
	ProtocolIO                 ProtocolErrorKind = "io"
	ProtocolTruncated          ProtocolErrorKind = "truncated"
	ProtocolTrailingBytes      ProtocolErrorKind = "trailing-bytes"
	ProtocolInvalidMagic       ProtocolErrorKind = "invalid-magic"
	ProtocolUnsupportedVersion ProtocolErrorKind = "unsupported-version"
	ProtocolUnknownMessage     ProtocolErrorKind = "unknown-message"
	ProtocolReservedBits       ProtocolErrorKind = "reserved-bits"
	ProtocolFrameTooLarge      ProtocolErrorKind = "frame-too-large"
	ProtocolChecksum           ProtocolErrorKind = "checksum"
	ProtocolInvalidField       ProtocolErrorKind = "invalid-field"
	ProtocolInvalidEnum        ProtocolErrorKind = "invalid-enum"
)

// ProtocolError has a stable Kind that callers and tests can match.
type ProtocolError struct {
	Kind   ProtocolErrorKind
	Field  string
	Value  uint64
	Detail string
	Err    error
}

func (e *ProtocolError) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("ftwdb shadow protocol %s: %v", e.Kind, e.Err)
	case e.Field != "" && e.Detail != "":
		return fmt.Sprintf("ftwdb shadow protocol %s for %s: %s", e.Kind, e.Field, e.Detail)
	case e.Field != "":
		return fmt.Sprintf("ftwdb shadow protocol %s for %s", e.Kind, e.Field)
	case e.Detail != "":
		return fmt.Sprintf("ftwdb shadow protocol %s: %s", e.Kind, e.Detail)
	default:
		return fmt.Sprintf("ftwdb shadow protocol %s", e.Kind)
	}
}

func (e *ProtocolError) Unwrap() error { return e.Err }

func protocolError(kind ProtocolErrorKind, field, detail string) error {
	return &ProtocolError{Kind: kind, Field: field, Detail: detail}
}

// Encode returns one checksummed v1 frame.
func Encode(message Message) ([]byte, error) {
	if message == nil {
		return nil, protocolError(ProtocolInvalidField, "message", "must not be nil")
	}
	payload := make([]byte, 0, 256)
	kind, err := encodePayload(&payload, message)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxPayloadBytes {
		return nil, frameTooLarge(uint64(len(payload) + headerBytes + checksumBytes))
	}
	frame := make([]byte, 0, headerBytes+len(payload)+checksumBytes)
	frame = append(frame, frameMagic[:]...)
	frame = binary.BigEndian.AppendUint16(frame, ProtocolVersion)
	frame = append(frame, byte(kind), 0)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
	return frame, nil
}

// Decode validates one complete v1 frame.
func Decode(frame []byte) (Message, error) {
	kind, total, err := parseHeader(frame)
	if err != nil {
		return nil, err
	}
	if len(frame) < total {
		return nil, &ProtocolError{
			Kind:   ProtocolTruncated,
			Detail: fmt.Sprintf("expected %d bytes, got %d", total, len(frame)),
		}
	}
	if len(frame) > total {
		return nil, &ProtocolError{
			Kind:   ProtocolTrailingBytes,
			Detail: fmt.Sprintf("%d extra bytes", len(frame)-total),
		}
	}
	actual := binary.BigEndian.Uint32(frame[total-checksumBytes:])
	expected := crc32.ChecksumIEEE(frame[:total-checksumBytes])
	if actual != expected {
		return nil, &ProtocolError{
			Kind:   ProtocolChecksum,
			Detail: fmt.Sprintf("expected %#08x, got %#08x", expected, actual),
		}
	}
	return decodePayload(kind, frame[headerBytes:total-checksumBytes])
}

// ReadMessage reads one bounded frame. It checks the header before allocating
// the payload.
func ReadMessage(reader io.Reader) (Message, error) {
	var header [headerBytes]byte
	if err := readExact(reader, header[:], 0); err != nil {
		return nil, err
	}
	_, total, err := parseHeader(header[:])
	if err != nil {
		return nil, err
	}
	frame := make([]byte, total)
	copy(frame, header[:])
	if err := readExact(reader, frame[headerBytes:], headerBytes); err != nil {
		return nil, err
	}
	return Decode(frame)
}

func WriteMessage(writer io.Writer, message Message) error {
	frame, err := Encode(message)
	if err != nil {
		return err
	}
	if err := writeAll(writer, frame); err != nil {
		return &ProtocolError{Kind: ProtocolIO, Err: err}
	}
	return nil
}

func parseHeader(frame []byte) (messageKind, int, error) {
	if len(frame) < headerBytes {
		return 0, 0, &ProtocolError{
			Kind:   ProtocolTruncated,
			Detail: fmt.Sprintf("expected %d bytes, got %d", headerBytes, len(frame)),
		}
	}
	if string(frame[:4]) != string(frameMagic[:]) {
		return 0, 0, protocolError(ProtocolInvalidMagic, "", "")
	}
	version := binary.BigEndian.Uint16(frame[4:6])
	if version != ProtocolVersion {
		return 0, 0, &ProtocolError{
			Kind:   ProtocolUnsupportedVersion,
			Value:  uint64(version),
			Detail: fmt.Sprintf("version %d", version),
		}
	}
	kind := messageKind(frame[6])
	if !validMessageKind(kind) {
		return 0, 0, &ProtocolError{
			Kind:   ProtocolUnknownMessage,
			Value:  uint64(kind),
			Detail: fmt.Sprintf("kind %d", kind),
		}
	}
	if frame[7] != 0 {
		return 0, 0, &ProtocolError{
			Kind:   ProtocolReservedBits,
			Value:  uint64(frame[7]),
			Detail: fmt.Sprintf("reserved byte %#02x", frame[7]),
		}
	}
	payloadLength := binary.BigEndian.Uint32(frame[8:12])
	if uint64(payloadLength) > uint64(maxPayloadBytes) {
		return 0, 0, frameTooLarge(uint64(payloadLength) + headerBytes + checksumBytes)
	}
	payload := int(payloadLength)
	return kind, payload + headerBytes + checksumBytes, nil
}

func validMessageKind(kind messageKind) bool {
	switch kind {
	case kindHelloRequest, kindCommitBatchRequest, kindFlushRequest, kindHealthRequest,
		kindHelloResponse, kindAckResponse, kindHealthResponse, kindErrorResponse:
		return true
	default:
		return false
	}
}

func frameTooLarge(size uint64) error {
	return &ProtocolError{
		Kind:   ProtocolFrameTooLarge,
		Detail: fmt.Sprintf("declared %d bytes; maximum is %d", size, MaxFrameBytes),
	}
}

func encodePayload(out *[]byte, message Message) (messageKind, error) {
	switch value := message.(type) {
	case HelloRequest:
		if value.SourceID.IsZero() {
			return 0, invalidField("source_id")
		}
		putID(out, value.SourceID)
		if err := putString(out, value.NodeID, 128, "node_id", true); err != nil {
			return 0, err
		}
		if err := putString(out, value.ClientVersion, 64, "client_version", true); err != nil {
			return 0, err
		}
		putUint64(out, value.Capabilities)
		return kindHelloRequest, nil
	case *HelloRequest:
		if value == nil {
			return 0, invalidField("message")
		}
		return encodePayload(out, *value)
	case CommitBatchRequest:
		if err := validateBatch(value); err != nil {
			return 0, err
		}
		putID(out, value.SourceID)
		putUint64(out, value.Sequence)
		putID(out, value.CommitID)
		putUint32(out, uint32(len(value.Entities)))
		for _, entity := range value.Entities {
			if err := encodeEntity(out, entity); err != nil {
				return 0, err
			}
		}
		putUint32(out, uint32(len(value.Relations)))
		for _, relation := range value.Relations {
			if err := encodeRelation(out, relation); err != nil {
				return 0, err
			}
		}
		putUint32(out, uint32(len(value.Series)))
		for _, series := range value.Series {
			if err := encodeSeries(out, series); err != nil {
				return 0, err
			}
		}
		putUint32(out, uint32(len(value.Runs)))
		for _, run := range value.Runs {
			if err := encodeRun(out, run); err != nil {
				return 0, err
			}
		}
		putUint32(out, uint32(len(value.Plans)))
		for _, plan := range value.Plans {
			if err := encodePlan(out, plan); err != nil {
				return 0, err
			}
		}
		putUint32(out, uint32(len(value.Points)))
		for _, point := range value.Points {
			encodePoint(out, point)
		}
		return kindCommitBatchRequest, nil
	case *CommitBatchRequest:
		if value == nil {
			return 0, invalidField("message")
		}
		return encodePayload(out, *value)
	case FlushRequest:
		if value.SourceID.IsZero() {
			return 0, invalidField("source_id")
		}
		putID(out, value.SourceID)
		putUint64(out, value.ThroughSequence)
		return kindFlushRequest, nil
	case *FlushRequest:
		if value == nil {
			return 0, invalidField("message")
		}
		return encodePayload(out, *value)
	case HealthRequest:
		putUint64(out, value.Nonce)
		return kindHealthRequest, nil
	case *HealthRequest:
		if value == nil {
			return 0, invalidField("message")
		}
		return encodePayload(out, *value)
	case HelloResponse:
		if value.SelectedVersion != ProtocolVersion {
			return 0, &ProtocolError{
				Kind:   ProtocolUnsupportedVersion,
				Value:  uint64(value.SelectedVersion),
				Detail: fmt.Sprintf("version %d", value.SelectedVersion),
			}
		}
		putUint16(out, value.SelectedVersion)
		putID(out, value.SessionID)
		putInt64(out, value.ServerTimeMicros)
		return kindHelloResponse, nil
	case *HelloResponse:
		if value == nil {
			return 0, invalidField("message")
		}
		return encodePayload(out, *value)
	case Ack:
		if value.Kind != AckCommitBatch && value.Kind != AckFlush {
			return 0, invalidEnum("ack kind", byte(value.Kind))
		}
		if value.SourceID.IsZero() {
			return 0, invalidField("source_id")
		}
		*out = append(*out, byte(value.Kind))
		putID(out, value.SourceID)
		putUint64(out, value.Sequence)
		putID(out, value.CommitID)
		putOptionalUint64(out, value.AcceptedThroughSequence)
		putOptionalUint64(out, value.DurableThroughSequence)
		putBool(out, value.Durable)
		putBool(out, value.Deduplicated)
		putUint64(out, value.FrameOffset)
		putUint32(out, value.Records)
		putUint32(out, value.Points)
		putUint64(out, value.BytesWritten)
		return kindAckResponse, nil
	case *Ack:
		if value == nil {
			return 0, invalidField("message")
		}
		return encodePayload(out, *value)
	case HealthResponse:
		if value.SourceID.IsZero() {
			return 0, invalidField("source_id")
		}
		if !validHealthStatus(value.Status) {
			return 0, invalidEnum("health status", byte(value.Status))
		}
		if value.QueueEntries > MaxQueueEntries {
			return 0, invalidField("queue_entries")
		}
		putUint64(out, value.Nonce)
		putID(out, value.SourceID)
		*out = append(*out, byte(value.Status))
		putUint32(out, value.QueueEntries)
		putOptionalUint64(out, value.AcceptedThroughSequence)
		putOptionalUint64(out, value.DurableThroughSequence)
		if value.Ops != nil {
			if err := encodeHealthOps(out, *value.Ops); err != nil {
				return 0, err
			}
		}
		return kindHealthResponse, nil
	case *HealthResponse:
		if value == nil {
			return 0, invalidField("message")
		}
		return encodePayload(out, *value)
	case ErrorResponse:
		if !validErrorCode(value.Code) {
			return 0, invalidEnum("error code", byte(value.Code))
		}
		*out = append(*out, byte(value.Code))
		putBool(out, value.Retryable)
		if err := putString(out, value.Message, maxErrorTextBytes, "error message", true); err != nil {
			return 0, err
		}
		return kindErrorResponse, nil
	case *ErrorResponse:
		if value == nil {
			return 0, invalidField("message")
		}
		return encodePayload(out, *value)
	default:
		return 0, protocolError(ProtocolInvalidField, "message", fmt.Sprintf("unsupported type %T", message))
	}
}

func decodePayload(kind messageKind, payload []byte) (Message, error) {
	in := input{data: payload}
	var message Message
	var err error
	switch kind {
	case kindHelloRequest:
		var value HelloRequest
		if value.SourceID, err = in.id(); err == nil {
			value.NodeID, err = in.string(128, "node_id", true)
		}
		if err == nil {
			value.ClientVersion, err = in.string(64, "client_version", true)
		}
		if err == nil {
			value.Capabilities, err = in.uint64()
		}
		if err == nil && value.SourceID.IsZero() {
			err = invalidField("source_id")
		}
		message = value
	case kindCommitBatchRequest:
		var value CommitBatchRequest
		value, err = decodeBatch(&in)
		message = value
	case kindFlushRequest:
		var value FlushRequest
		if value.SourceID, err = in.id(); err == nil {
			value.ThroughSequence, err = in.uint64()
		}
		if err == nil && value.SourceID.IsZero() {
			err = invalidField("source_id")
		}
		message = value
	case kindHealthRequest:
		var value HealthRequest
		value.Nonce, err = in.uint64()
		message = value
	case kindHelloResponse:
		var value HelloResponse
		if value.SelectedVersion, err = in.uint16(); err == nil && value.SelectedVersion != ProtocolVersion {
			err = &ProtocolError{
				Kind:   ProtocolUnsupportedVersion,
				Value:  uint64(value.SelectedVersion),
				Detail: fmt.Sprintf("version %d", value.SelectedVersion),
			}
		}
		if err == nil {
			value.SessionID, err = in.id()
		}
		if err == nil {
			value.ServerTimeMicros, err = in.int64()
		}
		message = value
	case kindAckResponse:
		var value Ack
		var enum byte
		if enum, err = in.byte(); err == nil {
			value.Kind = AckKind(enum)
			if value.Kind != AckCommitBatch && value.Kind != AckFlush {
				err = invalidEnum("ack kind", enum)
			}
		}
		if err == nil {
			value.SourceID, err = in.id()
		}
		if err == nil {
			value.Sequence, err = in.uint64()
		}
		if err == nil {
			value.CommitID, err = in.id()
		}
		if err == nil {
			value.AcceptedThroughSequence, err = in.optionalUint64("accepted watermark")
		}
		if err == nil {
			value.DurableThroughSequence, err = in.optionalUint64("durable watermark")
		}
		if err == nil {
			value.Durable, err = in.boolean("durable")
		}
		if err == nil {
			value.Deduplicated, err = in.boolean("deduplicated")
		}
		if err == nil {
			value.FrameOffset, err = in.uint64()
		}
		if err == nil {
			value.Records, err = in.uint32()
		}
		if err == nil {
			value.Points, err = in.uint32()
		}
		if err == nil {
			value.BytesWritten, err = in.uint64()
		}
		if err == nil && value.SourceID.IsZero() {
			err = invalidField("source_id")
		}
		message = value
	case kindHealthResponse:
		var value HealthResponse
		if value.Nonce, err = in.uint64(); err == nil {
			value.SourceID, err = in.id()
		}
		var enum byte
		if err == nil {
			enum, err = in.byte()
			value.Status = HealthStatus(enum)
			if err == nil && !validHealthStatus(value.Status) {
				err = invalidEnum("health status", enum)
			}
		}
		if err == nil {
			value.QueueEntries, err = in.uint32()
		}
		if err == nil && value.QueueEntries > MaxQueueEntries {
			err = invalidField("queue_entries")
		}
		if err == nil {
			value.AcceptedThroughSequence, err = in.optionalUint64("accepted watermark")
		}
		if err == nil {
			value.DurableThroughSequence, err = in.optionalUint64("durable watermark")
		}
		if err == nil && value.SourceID.IsZero() {
			err = invalidField("source_id")
		}
		if err == nil && in.remaining() > 0 {
			value.Ops, err = decodeHealthOps(&in)
		}
		message = value
	case kindErrorResponse:
		var value ErrorResponse
		var enum byte
		if enum, err = in.byte(); err == nil {
			value.Code = ErrorCode(enum)
			if !validErrorCode(value.Code) {
				err = invalidEnum("error code", enum)
			}
		}
		if err == nil {
			value.Retryable, err = in.boolean("retryable")
		}
		if err == nil {
			value.Message, err = in.string(maxErrorTextBytes, "error message", true)
		}
		message = value
	default:
		panic("validated message kind was not decoded")
	}
	if err != nil {
		return nil, err
	}
	if err := in.finish(); err != nil {
		return nil, err
	}
	return message, nil
}

func validateBatch(value CommitBatchRequest) error {
	if value.SourceID.IsZero() {
		return invalidField("source_id")
	}
	metadata := 0
	for _, count := range [...]int{
		len(value.Entities),
		len(value.Relations),
		len(value.Series),
		len(value.Runs),
		len(value.Plans),
	} {
		if count > MaxMetadataRecords-metadata {
			return invalidField("too many metadata records")
		}
		metadata += count
	}
	if len(value.Points) > MaxBatchPoints {
		return invalidField("too many points")
	}
	if metadata == 0 && len(value.Points) == 0 {
		return invalidField("empty transaction")
	}
	for _, series := range value.Series {
		if err := validateSeries(series); err != nil {
			return err
		}
	}
	for _, plan := range value.Plans {
		if err := validatePlan(plan); err != nil {
			return err
		}
	}
	for _, point := range value.Points {
		if point.SeriesID == 0 || point.ValidTimeEnd < point.ValidTime || math.IsNaN(point.Value) || math.IsInf(point.Value, 0) {
			return invalidField("invalid point")
		}
	}
	return nil
}

func encodeEntity(out *[]byte, value Entity) error {
	putID(out, value.ID)
	if err := putString(out, value.Kind, maxKeyBytes, "entity kind", true); err != nil {
		return err
	}
	if err := putString(out, value.Name, MaxTextBytes, "entity name", true); err != nil {
		return err
	}
	putOptionalID(out, value.Parent)
	putInt64(out, value.ValidFrom)
	putOptionalInt64(out, value.ValidTo)
	return encodeProperties(out, value.Properties)
}

func decodeEntity(in *input) (Entity, error) {
	var value Entity
	var err error
	if value.ID, err = in.id(); err == nil {
		value.Kind, err = in.string(maxKeyBytes, "entity kind", true)
	}
	if err == nil {
		value.Name, err = in.string(MaxTextBytes, "entity name", true)
	}
	if err == nil {
		value.Parent, err = in.optionalID("entity parent")
	}
	if err == nil {
		value.ValidFrom, err = in.int64()
	}
	if err == nil {
		value.ValidTo, err = in.optionalInt64("entity valid_to")
	}
	if err == nil {
		value.Properties, err = decodeProperties(in)
	}
	return value, err
}

func encodeRelation(out *[]byte, value Relation) error {
	putID(out, value.ID)
	if err := putString(out, value.Kind, maxKeyBytes, "relation kind", true); err != nil {
		return err
	}
	putID(out, value.Source)
	putID(out, value.Target)
	putInt64(out, value.ValidFrom)
	putOptionalInt64(out, value.ValidTo)
	return encodeProperties(out, value.Properties)
}

func decodeRelation(in *input) (Relation, error) {
	var value Relation
	var err error
	if value.ID, err = in.id(); err == nil {
		value.Kind, err = in.string(maxKeyBytes, "relation kind", true)
	}
	if err == nil {
		value.Source, err = in.id()
	}
	if err == nil {
		value.Target, err = in.id()
	}
	if err == nil {
		value.ValidFrom, err = in.int64()
	}
	if err == nil {
		value.ValidTo, err = in.optionalInt64("relation valid_to")
	}
	if err == nil {
		value.Properties, err = decodeProperties(in)
	}
	return value, err
}

func encodeSeries(out *[]byte, value SeriesDefinition) error {
	if err := validateSeries(value); err != nil {
		return err
	}
	putUint64(out, value.ID)
	putOptionalID(out, value.OwnerEntity)
	putOptionalID(out, value.OwnerRelation)
	if err := putString(out, value.Name, maxKeyBytes, "series name", true); err != nil {
		return err
	}
	if err := putString(out, value.PhysicalQuantity, maxKeyBytes, "physical quantity", true); err != nil {
		return err
	}
	if err := putString(out, value.CanonicalUnit, maxKeyBytes, "canonical unit", true); err != nil {
		return err
	}
	*out = append(*out, byte(value.Semantics))
	putOptionalInt64(out, value.MaximumGapMicros)
	return encodeRollupPolicy(out, value.RollupPolicy)
}

func decodeSeries(in *input) (SeriesDefinition, error) {
	var value SeriesDefinition
	var err error
	if value.ID, err = in.uint64(); err == nil {
		value.OwnerEntity, err = in.optionalID("owner entity")
	}
	if err == nil {
		value.OwnerRelation, err = in.optionalID("owner relation")
	}
	if err == nil {
		value.Name, err = in.string(maxKeyBytes, "series name", true)
	}
	if err == nil {
		value.PhysicalQuantity, err = in.string(maxKeyBytes, "physical quantity", true)
	}
	if err == nil {
		value.CanonicalUnit, err = in.string(maxKeyBytes, "canonical unit", true)
	}
	var enum byte
	if err == nil {
		enum, err = in.byte()
		value.Semantics = SeriesSemantics(enum)
		if err == nil && !validSeriesSemantics(value.Semantics) {
			err = invalidEnum("series semantics", enum)
		}
	}
	if err == nil {
		value.MaximumGapMicros, err = in.optionalInt64("maximum gap")
	}
	if err == nil {
		value.RollupPolicy, err = decodeRollupPolicy(in)
	}
	if err == nil {
		err = validateSeries(value)
	}
	return value, err
}

func validateSeries(value SeriesDefinition) error {
	if value.ID == 0 {
		return invalidField("series id zero is reserved")
	}
	if strings.TrimSpace(value.Name) == "" {
		return invalidField("series name must not be empty")
	}
	if strings.TrimSpace(value.PhysicalQuantity) == "" || strings.TrimSpace(value.CanonicalUnit) == "" {
		return invalidField("physical quantity and canonical unit are required")
	}
	if (value.OwnerEntity == nil) == (value.OwnerRelation == nil) {
		return invalidField("series must belong to exactly one entity or relation")
	}
	if value.MaximumGapMicros != nil && *value.MaximumGapMicros < 0 {
		return invalidField("maximum gap must not be negative")
	}
	if !validSeriesSemantics(value.Semantics) {
		return invalidEnum("series semantics", byte(value.Semantics))
	}
	if len(value.RollupPolicy.Tiers) > MaxRollupTiers {
		return invalidField("too many rollup tiers")
	}
	for _, tier := range value.RollupPolicy.Tiers {
		switch tier.Resolution.Kind {
		case RollupFixedMicros:
			if tier.Resolution.FixedMicros <= 0 {
				return invalidField("fixed rollup resolution must be positive")
			}
		case RollupCalendar:
			if !validCalendarUnit(tier.Resolution.CalendarUnit) {
				return invalidEnum("calendar unit", byte(tier.Resolution.CalendarUnit))
			}
			if strings.TrimSpace(tier.Resolution.IANATimezone) == "" {
				return invalidField("calendar rollups require an IANA timezone")
			}
		default:
			return invalidEnum("rollup resolution", byte(tier.Resolution.Kind))
		}
		if tier.RetainForMicros != nil && *tier.RetainForMicros <= 0 {
			return invalidField("rollup retention must be positive or forever")
		}
	}
	return nil
}

func encodeRollupPolicy(out *[]byte, value RollupPolicy) error {
	if len(value.Tiers) > MaxRollupTiers {
		return invalidField("too many rollup tiers")
	}
	putOptionalInt64(out, value.RawRetainForMicros)
	putUint32(out, uint32(len(value.Tiers)))
	for _, tier := range value.Tiers {
		*out = append(*out, byte(tier.Resolution.Kind))
		switch tier.Resolution.Kind {
		case RollupFixedMicros:
			putInt64(out, tier.Resolution.FixedMicros)
		case RollupCalendar:
			*out = append(*out, byte(tier.Resolution.CalendarUnit))
			if err := putString(out, tier.Resolution.IANATimezone, maxKeyBytes, "IANA timezone", true); err != nil {
				return err
			}
		default:
			return invalidEnum("rollup resolution", byte(tier.Resolution.Kind))
		}
		putOptionalInt64(out, tier.RetainForMicros)
	}
	return nil
}

func decodeRollupPolicy(in *input) (RollupPolicy, error) {
	var value RollupPolicy
	var err error
	if value.RawRetainForMicros, err = in.optionalInt64("raw retention"); err != nil {
		return value, err
	}
	count, err := in.count(MaxRollupTiers, "rollup tier count")
	if err != nil {
		return value, err
	}
	value.Tiers = make([]RollupTier, 0, count)
	for range count {
		var tier RollupTier
		kind, err := in.byte()
		if err != nil {
			return value, err
		}
		tier.Resolution.Kind = RollupResolutionKind(kind)
		switch tier.Resolution.Kind {
		case RollupFixedMicros:
			tier.Resolution.FixedMicros, err = in.int64()
		case RollupCalendar:
			var enum byte
			if enum, err = in.byte(); err == nil {
				tier.Resolution.CalendarUnit = CalendarUnit(enum)
				if !validCalendarUnit(tier.Resolution.CalendarUnit) {
					err = invalidEnum("calendar unit", enum)
				}
			}
			if err == nil {
				tier.Resolution.IANATimezone, err = in.string(maxKeyBytes, "IANA timezone", true)
			}
		default:
			err = invalidEnum("rollup resolution", kind)
		}
		if err != nil {
			return value, err
		}
		tier.RetainForMicros, err = in.optionalInt64("tier retention")
		if err != nil {
			return value, err
		}
		value.Tiers = append(value.Tiers, tier)
	}
	return value, nil
}

func encodeRun(out *[]byte, value Run) error {
	if !validRunKind(value.Kind) {
		return invalidEnum("run kind", byte(value.Kind))
	}
	if !validRunStatus(value.Status) {
		return invalidEnum("run status", byte(value.Status))
	}
	putID(out, value.ID)
	*out = append(*out, byte(value.Kind), byte(value.Status))
	putInt64(out, value.CreatedAt)
	putInt64(out, value.KnowledgeTime)
	if err := putString(out, value.Workflow, MaxTextBytes, "workflow", true); err != nil {
		return err
	}
	if err := putString(out, value.Model, MaxTextBytes, "model", false); err != nil {
		return err
	}
	if err := putString(out, value.ModelVersion, MaxTextBytes, "model version", false); err != nil {
		return err
	}
	putOptionalID(out, value.ParentRun)
	putOptionalID(out, value.InputSnapshot)
	return encodeProperties(out, value.Attributes)
}

func decodeRun(in *input) (Run, error) {
	var value Run
	var err error
	if value.ID, err = in.id(); err == nil {
		var enum byte
		enum, err = in.byte()
		value.Kind = RunKind(enum)
		if err == nil && !validRunKind(value.Kind) {
			err = invalidEnum("run kind", enum)
		}
	}
	if err == nil {
		var enum byte
		enum, err = in.byte()
		value.Status = RunStatus(enum)
		if err == nil && !validRunStatus(value.Status) {
			err = invalidEnum("run status", enum)
		}
	}
	if err == nil {
		value.CreatedAt, err = in.int64()
	}
	if err == nil {
		value.KnowledgeTime, err = in.int64()
	}
	if err == nil {
		value.Workflow, err = in.string(MaxTextBytes, "workflow", true)
	}
	if err == nil {
		value.Model, err = in.string(MaxTextBytes, "model", false)
	}
	if err == nil {
		value.ModelVersion, err = in.string(MaxTextBytes, "model version", false)
	}
	if err == nil {
		value.ParentRun, err = in.optionalID("parent run")
	}
	if err == nil {
		value.InputSnapshot, err = in.optionalID("input snapshot")
	}
	if err == nil {
		value.Attributes, err = decodeProperties(in)
	}
	return value, err
}

func encodePlan(out *[]byte, value Plan) error {
	if err := validatePlan(value); err != nil {
		return err
	}
	putID(out, value.ID)
	putID(out, value.RunID)
	*out = append(*out, byte(value.Status))
	putInt64(out, value.HorizonStart)
	putInt64(out, value.HorizonEnd)
	putInt64(out, value.ResolutionMicros)
	if err := putString(out, value.Scenario, MaxTextBytes, "scenario", true); err != nil {
		return err
	}
	if len(value.ObjectiveTerms) > MaxProperties {
		return invalidField("too many objective terms")
	}
	keys := sortedKeys(value.ObjectiveTerms)
	putUint32(out, uint32(len(keys)))
	for _, key := range keys {
		if err := putString(out, key, maxKeyBytes, "objective key", true); err != nil {
			return err
		}
		if err := putFloat64(out, value.ObjectiveTerms[key], "objective value"); err != nil {
			return err
		}
	}
	if err := putOptionalFloat64(out, value.ObjectiveValue, "objective value"); err != nil {
		return err
	}
	putOptionalID(out, value.Supersedes)
	return encodeProperties(out, value.Attributes)
}

func decodePlan(in *input) (Plan, error) {
	var value Plan
	var err error
	if value.ID, err = in.id(); err == nil {
		value.RunID, err = in.id()
	}
	if err == nil {
		var enum byte
		enum, err = in.byte()
		value.Status = PlanStatus(enum)
		if err == nil && !validPlanStatus(value.Status) {
			err = invalidEnum("plan status", enum)
		}
	}
	if err == nil {
		value.HorizonStart, err = in.int64()
	}
	if err == nil {
		value.HorizonEnd, err = in.int64()
	}
	if err == nil {
		value.ResolutionMicros, err = in.int64()
	}
	if err == nil {
		value.Scenario, err = in.string(MaxTextBytes, "scenario", true)
	}
	if err != nil {
		return value, err
	}
	count, err := in.count(MaxProperties, "objective term count")
	if err != nil {
		return value, err
	}
	value.ObjectiveTerms = make(map[string]float64, count)
	previous := ""
	for index := range count {
		key, err := in.string(maxKeyBytes, "objective key", true)
		if err != nil {
			return value, err
		}
		if index > 0 && previous >= key {
			return value, invalidField("objective keys")
		}
		previous = key
		number, err := in.float64("objective value")
		if err != nil {
			return value, err
		}
		value.ObjectiveTerms[key] = number
	}
	if value.ObjectiveValue, err = in.optionalFloat64("objective value"); err == nil {
		value.Supersedes, err = in.optionalID("supersedes")
	}
	if err == nil {
		value.Attributes, err = decodeProperties(in)
	}
	if err == nil {
		err = validatePlan(value)
	}
	return value, err
}

func validatePlan(value Plan) error {
	if value.ID.IsZero() || value.RunID.IsZero() {
		return invalidField("plan and run ids must be non-zero")
	}
	if !validPlanStatus(value.Status) {
		return invalidEnum("plan status", byte(value.Status))
	}
	if value.HorizonEnd <= value.HorizonStart {
		return invalidField("plan horizon must have positive duration")
	}
	if value.ResolutionMicros <= 0 {
		return invalidField("plan resolution must be positive")
	}
	if strings.TrimSpace(value.Scenario) == "" {
		return invalidField("plan scenario must not be empty")
	}
	for _, number := range value.ObjectiveTerms {
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return invalidField("objective value")
		}
	}
	if value.ObjectiveValue != nil && (math.IsNaN(*value.ObjectiveValue) || math.IsInf(*value.ObjectiveValue, 0)) {
		return invalidField("objective value")
	}
	return nil
}

func encodeProperties(out *[]byte, values map[string]PropertyValue) error {
	if len(values) > MaxProperties {
		return invalidField("too many properties")
	}
	keys := sortedKeys(values)
	putUint32(out, uint32(len(keys)))
	for _, key := range keys {
		if err := putString(out, key, maxKeyBytes, "property key", true); err != nil {
			return err
		}
		value := values[key]
		*out = append(*out, byte(value.Kind))
		switch value.Kind {
		case PropertyNull:
		case PropertyBool:
			putBool(out, value.Bool)
		case PropertyInteger:
			putInt64(out, value.Integer)
		case PropertyFloat:
			if err := putFloat64(out, value.Float, "property float"); err != nil {
				return err
			}
		case PropertyText:
			if err := putString(out, value.Text, MaxTextBytes, "property text", false); err != nil {
				return err
			}
		default:
			return invalidEnum("property value", byte(value.Kind))
		}
	}
	return nil
}

func decodeProperties(in *input) (map[string]PropertyValue, error) {
	count, err := in.count(MaxProperties, "property count")
	if err != nil {
		return nil, err
	}
	values := make(map[string]PropertyValue, count)
	previous := ""
	for index := range count {
		key, err := in.string(maxKeyBytes, "property key", true)
		if err != nil {
			return nil, err
		}
		if index > 0 && previous >= key {
			return nil, invalidField("property keys")
		}
		previous = key
		var value PropertyValue
		enum, err := in.byte()
		if err != nil {
			return nil, err
		}
		value.Kind = PropertyKind(enum)
		switch value.Kind {
		case PropertyNull:
		case PropertyBool:
			value.Bool, err = in.boolean("property bool")
		case PropertyInteger:
			value.Integer, err = in.int64()
		case PropertyFloat:
			value.Float, err = in.float64("property float")
		case PropertyText:
			value.Text, err = in.string(MaxTextBytes, "property text", false)
		default:
			err = invalidEnum("property value", enum)
		}
		if err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, nil
}

func decodeBatch(in *input) (CommitBatchRequest, error) {
	var value CommitBatchRequest
	var err error
	if value.SourceID, err = in.id(); err == nil {
		value.Sequence, err = in.uint64()
	}
	if err == nil {
		value.CommitID, err = in.id()
	}
	if err != nil {
		return value, err
	}
	remaining := MaxMetadataRecords
	if value.Entities, err = decodeCollection(in, &remaining, decodeEntity); err != nil {
		return value, err
	}
	if value.Relations, err = decodeCollection(in, &remaining, decodeRelation); err != nil {
		return value, err
	}
	if value.Series, err = decodeCollection(in, &remaining, decodeSeries); err != nil {
		return value, err
	}
	if value.Runs, err = decodeCollection(in, &remaining, decodeRun); err != nil {
		return value, err
	}
	if value.Plans, err = decodeCollection(in, &remaining, decodePlan); err != nil {
		return value, err
	}
	count, err := in.count(MaxBatchPoints, "point count")
	if err != nil {
		return value, err
	}
	if in.remaining() != count*pointBytes {
		return value, invalidField("point payload length")
	}
	value.Points = make([]Point, 0, count)
	for range count {
		point, err := decodePoint(in)
		if err != nil {
			return value, err
		}
		value.Points = append(value.Points, point)
	}
	if err := validateBatch(value); err != nil {
		return value, err
	}
	return value, nil
}

func decodeCollection[T any](in *input, remaining *int, decode func(*input) (T, error)) ([]T, error) {
	count, err := in.count(*remaining, "metadata record count")
	if err != nil {
		return nil, err
	}
	*remaining -= count
	values := make([]T, 0, count)
	for range count {
		value, err := decode(in)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func encodePoint(out *[]byte, value Point) {
	putUint64(out, value.SeriesID)
	putInt64(out, value.ValidTime)
	putInt64(out, value.ValidTimeEnd)
	putInt64(out, value.KnowledgeTime)
	putInt64(out, value.ChangeTime)
	putID(out, value.RunID)
	putUint64(out, math.Float64bits(value.Value))
	putUint32(out, value.Quality)
	putUint32(out, value.Flags)
}

func decodePoint(in *input) (Point, error) {
	var value Point
	var err error
	if value.SeriesID, err = in.uint64(); err == nil {
		value.ValidTime, err = in.int64()
	}
	if err == nil {
		value.ValidTimeEnd, err = in.int64()
	}
	if err == nil {
		value.KnowledgeTime, err = in.int64()
	}
	if err == nil {
		value.ChangeTime, err = in.int64()
	}
	if err == nil {
		value.RunID, err = in.id()
	}
	var bits uint64
	if err == nil {
		bits, err = in.uint64()
		value.Value = math.Float64frombits(bits)
	}
	if err == nil {
		value.Quality, err = in.uint32()
	}
	if err == nil {
		value.Flags, err = in.uint32()
	}
	return value, err
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func validSeriesSemantics(value SeriesSemantics) bool {
	return value >= SeriesGauge && value <= SeriesEvent
}

func validCalendarUnit(value CalendarUnit) bool {
	return value >= CalendarDay && value <= CalendarYear
}

func validRunKind(value RunKind) bool {
	return value >= RunForecast && value <= RunReconciliation
}

func validRunStatus(value RunStatus) bool {
	return value >= RunPending && value <= RunCancelled
}

func validPlanStatus(value PlanStatus) bool {
	return value >= PlanCandidate && value <= PlanCancelled
}

func validHealthStatus(value HealthStatus) bool {
	return value >= HealthHealthy && value <= HealthUnavailable
}

func validErrorCode(value ErrorCode) bool {
	return value >= ErrorInvalidRequest && value <= ErrorIdempotencyConflict
}

func invalidField(field string) error {
	return protocolError(ProtocolInvalidField, field, "")
}

func invalidEnum(field string, value byte) error {
	return &ProtocolError{
		Kind:   ProtocolInvalidEnum,
		Field:  field,
		Value:  uint64(value),
		Detail: fmt.Sprintf("value %d", value),
	}
}

func putString(out *[]byte, value string, maximum int, field string, required bool) error {
	if !utf8.ValidString(value) || len(value) > maximum || len(value) > math.MaxUint16 || required && value == "" {
		return invalidField(field)
	}
	added := 2 + len(value)
	if len(*out) > maxPayloadBytes-added {
		return frameTooLarge(uint64(len(*out) + added + headerBytes + checksumBytes))
	}
	putUint16(out, uint16(len(value)))
	*out = append(*out, value...)
	return nil
}

func putOptionalID(out *[]byte, value *ID128) {
	if value == nil {
		*out = append(*out, 0)
		return
	}
	*out = append(*out, 1)
	putID(out, *value)
}

func putOptionalInt64(out *[]byte, value *int64) {
	if value == nil {
		*out = append(*out, 0)
		return
	}
	*out = append(*out, 1)
	putInt64(out, *value)
}

func putOptionalUint64(out *[]byte, value *uint64) {
	if value == nil {
		*out = append(*out, 0)
		return
	}
	*out = append(*out, 1)
	putUint64(out, *value)
}

func putOptionalFloat64(out *[]byte, value *float64, field string) error {
	if value == nil {
		*out = append(*out, 0)
		return nil
	}
	*out = append(*out, 1)
	return putFloat64(out, *value, field)
}

func putFloat64(out *[]byte, value float64, field string) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return invalidField(field)
	}
	putUint64(out, math.Float64bits(value))
	return nil
}

func putBool(out *[]byte, value bool) {
	if value {
		*out = append(*out, 1)
	} else {
		*out = append(*out, 0)
	}
}

func putID(out *[]byte, value ID128) {
	*out = append(*out, value[:]...)
}

func putUint16(out *[]byte, value uint16) {
	*out = binary.BigEndian.AppendUint16(*out, value)
}

func putUint32(out *[]byte, value uint32) {
	*out = binary.BigEndian.AppendUint32(*out, value)
}

func putUint64(out *[]byte, value uint64) {
	*out = binary.BigEndian.AppendUint64(*out, value)
}

func putInt64(out *[]byte, value int64) {
	putUint64(out, uint64(value))
}

func readExact(reader io.Reader, target []byte, base int) error {
	read := 0
	for read < len(target) {
		count, err := reader.Read(target[read:])
		if count < 0 || count > len(target)-read {
			return &ProtocolError{Kind: ProtocolIO, Err: io.ErrShortBuffer}
		}
		read += count
		if read == len(target) {
			return nil
		}
		if err == nil {
			if count == 0 {
				return &ProtocolError{
					Kind:   ProtocolTruncated,
					Detail: fmt.Sprintf("expected %d bytes, got %d", base+len(target), base+read),
				}
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			return &ProtocolError{
				Kind:   ProtocolTruncated,
				Detail: fmt.Sprintf("expected %d bytes, got %d", base+len(target), base+read),
			}
		}
		return &ProtocolError{Kind: ProtocolIO, Err: err}
	}
	return nil
}

type input struct {
	data     []byte
	position int
}

func (in *input) take(count int) ([]byte, error) {
	if count < 0 || count > len(in.data)-in.position {
		expected := in.position + count
		if count < 0 {
			expected = math.MaxInt
		}
		return nil, &ProtocolError{
			Kind:   ProtocolTruncated,
			Detail: fmt.Sprintf("expected %d bytes, got %d", expected, len(in.data)),
		}
	}
	value := in.data[in.position : in.position+count]
	in.position += count
	return value, nil
}

func (in *input) finish() error {
	if in.position == len(in.data) {
		return nil
	}
	return &ProtocolError{
		Kind:   ProtocolTrailingBytes,
		Detail: fmt.Sprintf("%d extra bytes", len(in.data)-in.position),
	}
}

func (in *input) remaining() int {
	return len(in.data) - in.position
}

func (in *input) byte() (byte, error) {
	value, err := in.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (in *input) uint16() (uint16, error) {
	value, err := in.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(value), nil
}

func (in *input) uint32() (uint32, error) {
	value, err := in.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(value), nil
}

func (in *input) uint64() (uint64, error) {
	value, err := in.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func (in *input) int64() (int64, error) {
	value, err := in.uint64()
	return int64(value), err
}

func (in *input) id() (ID128, error) {
	var value ID128
	bytes, err := in.take(len(value))
	if err != nil {
		return value, err
	}
	copy(value[:], bytes)
	return value, nil
}

func (in *input) boolean(field string) (bool, error) {
	value, err := in.byte()
	if err != nil {
		return false, err
	}
	switch value {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, invalidEnum(field, value)
	}
}

func (in *input) string(maximum int, field string, required bool) (string, error) {
	size, err := in.uint16()
	if err != nil {
		return "", err
	}
	if int(size) > maximum || required && size == 0 {
		return "", invalidField(field)
	}
	value, err := in.take(int(size))
	if err != nil {
		return "", err
	}
	if !utf8.Valid(value) {
		return "", invalidField(field)
	}
	return string(value), nil
}

func (in *input) optionalID(field string) (*ID128, error) {
	present, err := in.byte()
	if err != nil {
		return nil, err
	}
	switch present {
	case 0:
		return nil, nil
	case 1:
		value, err := in.id()
		return &value, err
	default:
		return nil, invalidEnum(field, present)
	}
}

func (in *input) optionalInt64(field string) (*int64, error) {
	present, err := in.byte()
	if err != nil {
		return nil, err
	}
	switch present {
	case 0:
		return nil, nil
	case 1:
		value, err := in.int64()
		return &value, err
	default:
		return nil, invalidEnum(field, present)
	}
}

func (in *input) optionalUint64(field string) (*uint64, error) {
	present, err := in.byte()
	if err != nil {
		return nil, err
	}
	switch present {
	case 0:
		return nil, nil
	case 1:
		value, err := in.uint64()
		return &value, err
	default:
		return nil, invalidEnum(field, present)
	}
}

func (in *input) float64(field string) (float64, error) {
	bits, err := in.uint64()
	if err != nil {
		return 0, err
	}
	value := math.Float64frombits(bits)
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, invalidField(field)
	}
	return value, nil
}

func (in *input) optionalFloat64(field string) (*float64, error) {
	present, err := in.byte()
	if err != nil {
		return nil, err
	}
	switch present {
	case 0:
		return nil, nil
	case 1:
		value, err := in.float64(field)
		return &value, err
	default:
		return nil, invalidEnum(field, present)
	}
}

func (in *input) count(maximum int, field string) (int, error) {
	count, err := in.uint32()
	if err != nil {
		return 0, err
	}
	if uint64(count) > uint64(maximum) {
		return 0, invalidField(field)
	}
	return int(count), nil
}
