package ftwdbshadow

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeRejectsMalformedFramesBeforeUse(t *testing.T) {
	t.Parallel()
	valid := fixtureFrame(t, "health-request.hex")
	tests := []struct {
		name string
		edit func([]byte) []byte
		kind ProtocolErrorKind
	}{
		{
			name: "short header",
			edit: func(frame []byte) []byte { return frame[:headerBytes-1] },
			kind: ProtocolTruncated,
		},
		{
			name: "bad magic",
			edit: func(frame []byte) []byte {
				frame[0] ^= 0xff
				return frame
			},
			kind: ProtocolInvalidMagic,
		},
		{
			name: "unsupported version",
			edit: func(frame []byte) []byte {
				binary.BigEndian.PutUint16(frame[4:6], ProtocolVersion+1)
				return frame
			},
			kind: ProtocolUnsupportedVersion,
		},
		{
			name: "unknown kind",
			edit: func(frame []byte) []byte {
				frame[6] = 127
				return frame
			},
			kind: ProtocolUnknownMessage,
		},
		{
			name: "reserved bits",
			edit: func(frame []byte) []byte {
				frame[7] = 1
				return frame
			},
			kind: ProtocolReservedBits,
		},
		{
			name: "bad checksum",
			edit: func(frame []byte) []byte {
				frame[len(frame)-1] ^= 1
				return frame
			},
			kind: ProtocolChecksum,
		},
		{
			name: "trailing byte",
			edit: func(frame []byte) []byte { return append(frame, 0) },
			kind: ProtocolTrailingBytes,
		},
		{
			name: "short payload",
			edit: func(frame []byte) []byte { return frame[:len(frame)-1] },
			kind: ProtocolTruncated,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			frame := test.edit(append([]byte(nil), valid...))
			_, err := Decode(frame)
			requireProtocolKind(t, err, test.kind)
		})
	}
}

func TestDecodeRejectsOversizedHeaderBeforeAllocation(t *testing.T) {
	t.Parallel()
	header := make([]byte, headerBytes)
	copy(header, frameMagic[:])
	binary.BigEndian.PutUint16(header[4:6], ProtocolVersion)
	header[6] = byte(kindHealthRequest)
	binary.BigEndian.PutUint32(header[8:12], uint32(maxPayloadBytes+1))

	_, err := ReadMessage(bytes.NewReader(header))
	requireProtocolKind(t, err, ProtocolFrameTooLarge)
}

func TestReadMessageHandlesShortReads(t *testing.T) {
	t.Parallel()
	frame := fixtureFrame(t, "commit-batch-request.hex")
	message, err := ReadMessage(&oneByteReader{reader: bytes.NewReader(frame)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := message.(CommitBatchRequest); !ok {
		t.Fatalf("decoded %T, want CommitBatchRequest", message)
	}
}

func TestReadMessageAcceptsFinalBytesWithEOF(t *testing.T) {
	t.Parallel()
	frame := fixtureFrame(t, "health-response.hex")
	message, err := ReadMessage(&eofOnFinalReader{data: frame})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := message.(HealthResponse); !ok {
		t.Fatalf("decoded %T, want HealthResponse", message)
	}
}

func TestWriteMessageCompletesShortWrites(t *testing.T) {
	t.Parallel()
	message := HealthRequest{Nonce: 42}
	want, err := Encode(message)
	if err != nil {
		t.Fatal(err)
	}
	writer := &shortWriter{maximum: 3}
	if err := WriteMessage(writer, message); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(writer.Bytes(), want) {
		t.Fatalf("written bytes %x, want %x", writer.Bytes(), want)
	}
}

func TestPreparedCommitCopiesAndValidatesFrame(t *testing.T) {
	t.Parallel()
	frame := fixtureFrame(t, "commit-batch-request.hex")
	prepared, err := PreparedCommitFromFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	frame[0] ^= 0xff
	first := prepared.Bytes()
	first[1] ^= 0xff
	second := prepared.Bytes()
	if second[0] != 'F' || second[1] != 'T' {
		t.Fatal("prepared commit exposed mutable frame storage")
	}

	_, err = PreparedCommitFromFrame(fixtureFrame(t, "health-request.hex"))
	requireProtocolKind(t, err, ProtocolInvalidField)
}

func TestCodecRejectsNonCanonicalOrInvalidValues(t *testing.T) {
	t.Parallel()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	entity := mustID(t, "102030405060708090a0b0c0d0e0f001")
	tests := []struct {
		name    string
		message Message
	}{
		{
			name:    "zero source",
			message: HelloRequest{NodeID: "box", ClientVersion: "ftw"},
		},
		{
			name: "empty batch",
			message: CommitBatchRequest{
				SourceID: source,
				CommitID: entity,
			},
		},
		{
			name: "invalid point",
			message: CommitBatchRequest{
				SourceID: source,
				CommitID: entity,
				Points: []Point{{
					SeriesID:     1,
					ValidTime:    2,
					ValidTimeEnd: 1,
					Value:        1,
				}},
			},
		},
		{
			name: "bad utf8",
			message: HelloRequest{
				SourceID:      source,
				NodeID:        string([]byte{0xff}),
				ClientVersion: "ftw",
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Encode(test.message)
			requireProtocolKind(t, err, ProtocolInvalidField)
		})
	}
}

func TestDecodeRejectsInvalidBoolean(t *testing.T) {
	t.Parallel()
	frame := fixtureFrame(t, "error-response.hex")
	// Error payload starts with code, then the retryable byte.
	frame[headerBytes+1] = 2
	refreshChecksum(frame)
	_, err := Decode(frame)
	requireProtocolKind(t, err, ProtocolInvalidEnum)
}

func TestEncodeSortsMapsByWireBytes(t *testing.T) {
	t.Parallel()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	entity := mustID(t, "102030405060708090a0b0c0d0e0f001")
	batch := CommitBatchRequest{
		SourceID: source,
		CommitID: entity,
		Entities: []Entity{{
			ID:   entity,
			Kind: "site",
			Name: "box",
			Properties: map[string]PropertyValue{
				"z": IntegerProperty(1),
				"a": IntegerProperty(2),
			},
		}},
	}
	first, err := Encode(batch)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encode(batch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("map encoding is not deterministic")
	}
	if _, err := Decode(first); err != nil {
		t.Fatal(err)
	}
}

func TestEveryEnumTagRoundTrips(t *testing.T) {
	t.Parallel()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	for _, status := range []HealthStatus{HealthHealthy, HealthDegraded, HealthUnavailable} {
		message := roundTripMessage(t, HealthResponse{
			SourceID: source,
			Status:   status,
		}).(HealthResponse)
		if message.Status != status {
			t.Fatalf("health status %d became %d", status, message.Status)
		}
	}
	for _, code := range []ErrorCode{
		ErrorInvalidRequest,
		ErrorOverloaded,
		ErrorInternal,
		ErrorUnsupported,
		ErrorIdempotencyConflict,
	} {
		message := roundTripMessage(t, ErrorResponse{
			Code:    code,
			Message: "error",
		}).(ErrorResponse)
		if message.Code != code {
			t.Fatalf("error code %d became %d", code, message.Code)
		}
	}

	batch := fixtureMessages(t)["commit-batch-request.hex"].(CommitBatchRequest)
	for _, semantics := range []SeriesSemantics{
		SeriesGauge,
		SeriesIntervalTotal,
		SeriesCounter,
		SeriesState,
		SeriesEvent,
	} {
		batch.Series[0].Semantics = semantics
		message := roundTripMessage(t, batch).(CommitBatchRequest)
		if message.Series[0].Semantics != semantics {
			t.Fatalf("series semantics %d became %d", semantics, message.Series[0].Semantics)
		}
	}
	for _, unit := range []CalendarUnit{CalendarDay, CalendarMonth, CalendarYear} {
		batch.Series[0].RollupPolicy.Tiers[1].Resolution.CalendarUnit = unit
		message := roundTripMessage(t, batch).(CommitBatchRequest)
		if message.Series[0].RollupPolicy.Tiers[1].Resolution.CalendarUnit != unit {
			t.Fatalf("calendar unit %d changed", unit)
		}
	}
	for _, kind := range []RunKind{
		RunForecast,
		RunOptimization,
		RunImport,
		RunControl,
		RunReconciliation,
	} {
		batch.Runs[0].Kind = kind
		message := roundTripMessage(t, batch).(CommitBatchRequest)
		if message.Runs[0].Kind != kind {
			t.Fatalf("run kind %d changed", kind)
		}
	}
	for _, status := range []RunStatus{
		RunPending,
		RunRunning,
		RunSucceeded,
		RunFailed,
		RunCancelled,
	} {
		batch.Runs[0].Status = status
		message := roundTripMessage(t, batch).(CommitBatchRequest)
		if message.Runs[0].Status != status {
			t.Fatalf("run status %d changed", status)
		}
	}
	for _, status := range []PlanStatus{
		PlanCandidate,
		PlanApproved,
		PlanDeployed,
		PlanSuperseded,
		PlanCancelled,
	} {
		batch.Plans[0].Status = status
		message := roundTripMessage(t, batch).(CommitBatchRequest)
		if message.Plans[0].Status != status {
			t.Fatalf("plan status %d changed", status)
		}
	}
}

func roundTripMessage(t *testing.T, message Message) Message {
	t.Helper()
	frame, err := Encode(message)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(frame)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func fixtureFrame(t *testing.T, name string) []byte {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(fixtureDirectory, name))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func requireProtocolKind(t *testing.T, err error, want ProtocolErrorKind) {
	t.Helper()
	var protocol *ProtocolError
	if !errors.As(err, &protocol) {
		t.Fatalf("error %v has type %T, want ProtocolError", err, err)
	}
	if protocol.Kind != want {
		t.Fatalf("protocol error kind %q, want %q: %v", protocol.Kind, want, err)
	}
}

func refreshChecksum(frame []byte) {
	sum := crc32.ChecksumIEEE(frame[:len(frame)-checksumBytes])
	binary.BigEndian.PutUint32(frame[len(frame)-checksumBytes:], sum)
}

type oneByteReader struct{ reader io.Reader }

func (reader *oneByteReader) Read(value []byte) (int, error) {
	if len(value) > 1 {
		value = value[:1]
	}
	return reader.reader.Read(value)
}

type eofOnFinalReader struct{ data []byte }

func (reader *eofOnFinalReader) Read(value []byte) (int, error) {
	count := copy(value, reader.data)
	reader.data = reader.data[count:]
	if len(reader.data) == 0 {
		return count, io.EOF
	}
	return count, nil
}

type shortWriter struct {
	bytes.Buffer
	maximum int
}

func (writer *shortWriter) Write(value []byte) (int, error) {
	if len(value) > writer.maximum {
		value = value[:writer.maximum]
	}
	return writer.Buffer.Write(value)
}
