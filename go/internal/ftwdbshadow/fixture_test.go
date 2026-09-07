package ftwdbshadow

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const fixtureDirectory = "testdata/shadow-protocol-v1"

func TestV1GoldenFixtures(t *testing.T) {
	t.Parallel()
	for name, want := range fixtureMessages(t) {
		name, want := name, want
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			text, err := os.ReadFile(filepath.Join(fixtureDirectory, name))
			if err != nil {
				t.Fatal(err)
			}
			frame, err := hex.DecodeString(strings.TrimSpace(string(text)))
			if err != nil {
				t.Fatalf("decode fixture hex: %v", err)
			}
			got, err := Decode(frame)
			if err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded message mismatch\n got: %#v\nwant: %#v", got, want)
			}
			encoded, err := Encode(want)
			if err != nil {
				t.Fatalf("encode message: %v", err)
			}
			if !bytes.Equal(encoded, frame) {
				t.Fatalf("encoded bytes differ\n got: %x\nwant: %x", encoded, frame)
			}
		})
	}
}

func TestVendoredFixtureHashes(t *testing.T) {
	t.Parallel()
	file, err := os.Open(filepath.Join(fixtureDirectory, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			t.Fatalf("invalid SHA256SUMS line %q", scanner.Text())
		}
		want, err := hex.DecodeString(fields[0])
		if err != nil || len(want) != sha256.Size {
			t.Fatalf("invalid digest for %s", fields[1])
		}
		value, err := os.ReadFile(filepath.Join(fixtureDirectory, fields[1]))
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.Sum256(value)
		if !bytes.Equal(got[:], want) {
			t.Fatalf("%s digest %x, want %x", fields[1], got, want)
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != len(fixtureMessages(t)) {
		t.Fatalf("manifest contains %d frames, want %d", count, len(fixtureMessages(t)))
	}
}

func TestVendoredFixturesMatchSiblingFTWDB(t *testing.T) {
	t.Parallel()
	source := os.Getenv("FTWDB_SHADOW_FIXTURES")
	if source == "" {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("find fixture test source")
		}
		source = filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "ftwdb", "testdata", "shadow-protocol-v1"))
	}
	if _, err := os.Stat(source); err != nil {
		t.Skipf("sibling FTWDB fixtures are not present: %v", err)
	}
	localEntries, err := os.ReadDir(fixtureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	sourceEntries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	if names(localEntries) != names(sourceEntries) {
		t.Fatalf("fixture file sets differ\nlocal:  %s\nsource: %s", names(localEntries), names(sourceEntries))
	}
	for _, entry := range localEntries {
		if entry.IsDir() {
			continue
		}
		local, err := os.ReadFile(filepath.Join(fixtureDirectory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		upstream, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(local, upstream) {
			t.Fatalf("vendored fixture %s differs from %s", entry.Name(), source)
		}
	}
}

func names(entries []os.DirEntry) string {
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		values = append(values, entry.Name())
	}
	return strings.Join(values, "\n")
}

func fixtureMessages(t *testing.T) map[string]Message {
	t.Helper()
	sourceID := mustID(t, "00112233445566778899aabbccddeeff")
	commitID := mustID(t, "ffeeddccbbaa99887766554433221100")
	entityID := mustID(t, "102030405060708090a0b0c0d0e0f001")
	targetID := mustID(t, "102030405060708090a0b0c0d0e0f002")
	relationID := mustID(t, "2030405060708090a0b0c0d0e0f00102")
	runID := mustID(t, "30405060708090a0b0c0d0e0f0010203")
	sequence := uint64(0x0102030405060708)
	validFrom := int64(1_754_382_400_123_456)
	validTo := int64(1_754_468_800_123_456)
	maximumGap := int64(5_000_000)
	rawRetention := int64(1_209_600_000_000)
	tierRetention := int64(31_536_000_000_000)
	parentRun := mustID(t, "00000000000000000000000000000001")
	inputSnapshot := mustID(t, "00000000000000000000000000000002")
	supersedes := mustID(t, "00000000000000000000000000000003")
	objective := 12.25
	accepted := sequence
	durable := sequence

	batch := CommitBatchRequest{
		SourceID: sourceID,
		Sequence: sequence,
		CommitID: commitID,
		Entities: []Entity{{
			ID:        entityID,
			Kind:      "site",
			Name:      "FTW test box",
			ValidFrom: validFrom,
			ValidTo:   &validTo,
			Properties: map[string]PropertyValue{
				"bool":  BoolProperty(true),
				"float": FloatProperty(-12.5),
				"int":   IntegerProperty(-42),
				"null":  NullProperty(),
				"text":  TextProperty("grid import"),
			},
		}},
		Relations: []Relation{{
			ID:        relationID,
			Kind:      "feeds",
			Source:    entityID,
			Target:    targetID,
			ValidFrom: validFrom,
			Properties: map[string]PropertyValue{
				"phase": TextProperty("L1"),
			},
		}},
		Series: []SeriesDefinition{{
			ID:               0x1122334455667788,
			OwnerEntity:      &entityID,
			Name:             "grid_power",
			PhysicalQuantity: "power",
			CanonicalUnit:    "W",
			Semantics:        SeriesGauge,
			MaximumGapMicros: &maximumGap,
			RollupPolicy: RollupPolicy{
				RawRetainForMicros: &rawRetention,
				Tiers: []RollupTier{
					{
						Resolution: RollupResolution{
							Kind:        RollupFixedMicros,
							FixedMicros: 300_000_000,
						},
						RetainForMicros: &tierRetention,
					},
					{
						Resolution: RollupResolution{
							Kind:         RollupCalendar,
							CalendarUnit: CalendarDay,
							IANATimezone: "Europe/Stockholm",
						},
					},
				},
			},
		}},
		Runs: []Run{{
			ID:            runID,
			Kind:          RunOptimization,
			Status:        RunSucceeded,
			CreatedAt:     1_754_382_300_000_000,
			KnowledgeTime: 1_754_382_350_000_000,
			Workflow:      "day-ahead",
			Model:         "ftw-plan",
			ModelVersion:  "2026.08",
			ParentRun:     &parentRun,
			InputSnapshot: &inputSnapshot,
			Attributes: map[string]PropertyValue{
				"tariff": TextProperty("SE4"),
			},
		}},
		Plans: []Plan{{
			ID:               mustID(t, "405060708090a0b0c0d0e0f001020304"),
			RunID:            runID,
			Status:           PlanDeployed,
			HorizonStart:     1_754_382_400_000_000,
			HorizonEnd:       1_754_468_800_000_000,
			ResolutionMicros: 300_000_000,
			Scenario:         "base",
			ObjectiveTerms: map[string]float64{
				"cost_sek": 12.25,
				"peak_w":   4_500,
			},
			ObjectiveValue: &objective,
			Supersedes:     &supersedes,
			Attributes: map[string]PropertyValue{
				"mode": TextProperty("auto"),
			},
		}},
		Points: []Point{{
			SeriesID:      0x1122334455667788,
			ValidTime:     validFrom,
			ValidTimeEnd:  1_754_382_700_123_456,
			KnowledgeTime: 1_754_382_350_000_000,
			ChangeTime:    1_754_382_351_000_000,
			RunID:         runID,
			Value:         -1_234.5,
			Quality:       0x10203040,
			Flags:         0x50607080,
		}},
	}

	return map[string]Message{
		"hello-request.hex": HelloRequest{
			SourceID:      sourceID,
			NodeID:        "ftw-box-01",
			ClientVersion: "go-ftw/0.1.0",
			Capabilities:  sequence,
		},
		"commit-batch-request.hex": batch,
		"flush-request.hex": FlushRequest{
			SourceID:        sourceID,
			ThroughSequence: sequence,
		},
		"health-request.hex": HealthRequest{Nonce: 0x1122334455667788},
		"hello-response.hex": HelloResponse{
			SelectedVersion:  ProtocolVersion,
			SessionID:        sourceID,
			ServerTimeMicros: validFrom,
		},
		"commit-ack-response.hex": Ack{
			Kind:                    AckCommitBatch,
			SourceID:                sourceID,
			Sequence:                sequence,
			CommitID:                commitID,
			AcceptedThroughSequence: &accepted,
			DurableThroughSequence:  &durable,
			Durable:                 true,
			FrameOffset:             0x1112131415161718,
			Records:                 6,
			Points:                  1,
			BytesWritten:            0x2122232425262728,
		},
		"flush-ack-response.hex": Ack{
			Kind:                    AckFlush,
			SourceID:                sourceID,
			Sequence:                sequence,
			AcceptedThroughSequence: &accepted,
			DurableThroughSequence:  &durable,
			Durable:                 true,
		},
		"health-response.hex": HealthResponse{
			Nonce:                   0x1122334455667788,
			SourceID:                sourceID,
			Status:                  HealthDegraded,
			QueueEntries:            3,
			AcceptedThroughSequence: &accepted,
			Ops:                     &HealthOps{OverloadCount: 5, ProtocolErrorCount: 7, DatabaseBytes: 0x3132333435363738, DatabasePoints: 9, DatabaseCommits: 4, SyncPolicy: 1},
		},
		"error-response.hex": ErrorResponse{
			Code:      ErrorIdempotencyConflict,
			Retryable: false,
			Message:   "idempotency-conflict",
		},
	}
}

func mustID(t *testing.T, value string) ID128 {
	t.Helper()
	id, err := ParseID128(value)
	if err != nil {
		t.Fatalf("parse id %s: %v", value, err)
	}
	return id
}

func ExampleParseID128() {
	id, _ := ParseID128("00112233445566778899aabbccddeeff")
	fmt.Println(id)
	// Output: 00112233445566778899aabbccddeeff
}
