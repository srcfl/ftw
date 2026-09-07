//go:build !windows

package ftwdbshadow

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClientHealthBindsSource(t *testing.T) {
	t.Parallel()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	listener := listenUnix(t)
	server := runServer(listener, func(conn net.Conn) error {
		if err := serverHello(conn, source); err != nil {
			return err
		}
		message, err := ReadMessage(conn)
		if err != nil {
			return err
		}
		request, ok := message.(HealthRequest)
		if !ok {
			return fmt.Errorf("got %T, want HealthRequest", message)
		}
		accepted := uint64(90)
		durable := uint64(80)
		return WriteMessage(conn, HealthResponse{
			Nonce:                   request.Nonce,
			SourceID:                source,
			Status:                  HealthHealthy,
			QueueEntries:            2,
			AcceptedThroughSequence: &accepted,
			DurableThroughSequence:  &durable,
		})
	})

	client := connectTestClient(t, listener.Addr().String(), source, time.Second)
	defer client.Close()
	health, err := client.Health(context.Background(), 55)
	if err != nil {
		t.Fatal(err)
	}
	if health.Nonce != 55 || health.SourceID != source {
		t.Fatalf("unexpected health response: %#v", health)
	}
	if health.DurableThroughSequence == nil || *health.DurableThroughSequence != 80 {
		t.Fatalf("durable watermark %#v, want 80", health.DurableThroughSequence)
	}
	waitServer(t, server)
}

func TestClientCommitDurableFlushesNonDurableAck(t *testing.T) {
	t.Parallel()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	prepared := prepareTestCommit(t, source, 42)
	listener := listenUnix(t)
	server := runServer(listener, func(conn net.Conn) error {
		if err := serverHello(conn, source); err != nil {
			return err
		}
		frame, message, err := readRawFrame(conn)
		if err != nil {
			return err
		}
		if !bytes.Equal(frame, prepared.Bytes()) {
			return fmt.Errorf("commit frame differs from prepared bytes")
		}
		batch, ok := message.(CommitBatchRequest)
		if !ok {
			return fmt.Errorf("got %T, want CommitBatchRequest", message)
		}
		accepted := batch.Sequence
		if err := WriteMessage(conn, Ack{
			Kind:                    AckCommitBatch,
			SourceID:                source,
			Sequence:                batch.Sequence,
			CommitID:                batch.CommitID,
			AcceptedThroughSequence: &accepted,
			Points:                  1,
		}); err != nil {
			return err
		}
		message, err = ReadMessage(conn)
		if err != nil {
			return err
		}
		flush, ok := message.(FlushRequest)
		if !ok {
			return fmt.Errorf("got %T, want FlushRequest", message)
		}
		durable := flush.ThroughSequence
		return WriteMessage(conn, Ack{
			Kind:                    AckFlush,
			SourceID:                source,
			Sequence:                flush.ThroughSequence,
			AcceptedThroughSequence: &accepted,
			DurableThroughSequence:  &durable,
			Durable:                 true,
		})
	})

	client := connectTestClient(t, listener.Addr().String(), source, time.Second)
	defer client.Close()
	result, err := client.CommitDurable(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if result.Flush == nil {
		t.Fatal("non-durable commit did not cause a flush")
	}
	if result.DurableThrough != prepared.Sequence() {
		t.Fatalf("durable through %d, want %d", result.DurableThrough, prepared.Sequence())
	}
	waitServer(t, server)
}

func TestClientRejectsLocalSourceMismatchWithoutWriting(t *testing.T) {
	t.Parallel()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	other := mustID(t, "10112233445566778899aabbccddeeff")
	prepared := prepareTestCommit(t, other, 1)
	listener := listenUnix(t)
	server := runServer(listener, func(conn net.Conn) error {
		if err := serverHello(conn, source); err != nil {
			return err
		}
		message, err := ReadMessage(conn)
		if err != nil {
			return err
		}
		request, ok := message.(HealthRequest)
		if !ok {
			return fmt.Errorf("source-mismatched commit reached wire as %T", message)
		}
		return WriteMessage(conn, HealthResponse{
			Nonce:    request.Nonce,
			SourceID: source,
			Status:   HealthHealthy,
		})
	})

	client := connectTestClient(t, listener.Addr().String(), source, time.Second)
	defer client.Close()
	_, err := client.CommitPrepared(context.Background(), prepared)
	var clientError *ClientError
	if !errors.As(err, &clientError) || clientError.Kind != FailureContract || ShouldRetry(err) {
		t.Fatalf("source mismatch error = %#v, want stable contract failure", err)
	}
	if _, err := client.Health(context.Background(), 9); err != nil {
		t.Fatalf("connection was not reusable after local rejection: %v", err)
	}
	waitServer(t, server)
}

func TestClientKeepsRemoteRetryDecisionStable(t *testing.T) {
	t.Parallel()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	prepared := prepareTestCommit(t, source, 4)
	listener := listenUnix(t)
	server := runServer(listener, func(conn net.Conn) error {
		if err := serverHello(conn, source); err != nil {
			return err
		}
		if _, err := ReadMessage(conn); err != nil {
			return err
		}
		if err := WriteMessage(conn, ErrorResponse{
			Code:      ErrorOverloaded,
			Retryable: true,
			Message:   "shadow writer overloaded",
		}); err != nil {
			return err
		}
		message, err := ReadMessage(conn)
		if err != nil {
			return err
		}
		request, ok := message.(HealthRequest)
		if !ok {
			return fmt.Errorf("got %T, want HealthRequest", message)
		}
		return WriteMessage(conn, HealthResponse{
			Nonce:    request.Nonce,
			SourceID: source,
			Status:   HealthDegraded,
		})
	})

	client := connectTestClient(t, listener.Addr().String(), source, time.Second)
	defer client.Close()
	_, err := client.CommitPrepared(context.Background(), prepared)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != ErrorOverloaded || !ShouldRetry(err) {
		t.Fatalf("remote error = %#v, want retryable overload", err)
	}
	if _, err := client.Health(context.Background(), 3); err != nil {
		t.Fatalf("remote rejection broke stream: %v", err)
	}
	waitServer(t, server)
}

func TestClientFrameDeadlineIsAbsolute(t *testing.T) {
	source := mustID(t, "00112233445566778899aabbccddeeff")
	listener := listenUnix(t)
	server := runServer(listener, func(conn net.Conn) error {
		if err := serverHello(conn, source); err != nil {
			return err
		}
		message, err := ReadMessage(conn)
		if err != nil {
			return err
		}
		request, ok := message.(HealthRequest)
		if !ok {
			return fmt.Errorf("got %T, want HealthRequest", message)
		}
		frame, err := Encode(HealthResponse{
			Nonce:    request.Nonce,
			SourceID: source,
			Status:   HealthHealthy,
		})
		if err != nil {
			return err
		}
		for _, value := range frame {
			if _, err := conn.Write([]byte{value}); err != nil {
				return nil
			}
			time.Sleep(40 * time.Millisecond)
		}
		return nil
	})

	const timeout = 150 * time.Millisecond
	client := connectTestClient(t, listener.Addr().String(), source, timeout)
	defer client.Close()
	started := time.Now()
	_, err := client.Health(context.Background(), 1)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("slow frame passed the absolute deadline")
	}
	if !ShouldRetry(err) {
		t.Fatalf("deadline error is not retryable: %v", err)
	}
	if elapsed > 5*timeout {
		t.Fatalf("frame took %v, want an absolute deadline near %v", elapsed, timeout)
	}
	waitServer(t, server)
}

func TestClientContextCancellationInterruptsIO(t *testing.T) {
	t.Parallel()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	listener := listenUnix(t)
	requestSeen := make(chan struct{})
	server := runServer(listener, func(conn net.Conn) error {
		if err := serverHello(conn, source); err != nil {
			return err
		}
		if _, err := ReadMessage(conn); err != nil {
			return err
		}
		close(requestSeen)
		_, err := io.Copy(io.Discard, conn)
		return err
	})

	client := connectTestClient(t, listener.Addr().String(), source, 5*time.Second)
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Health(ctx, 1)
		result <- err
	}()
	<-requestSeen
	started := time.Now()
	cancel()
	select {
	case err := <-result:
		if err == nil || ShouldRetry(err) || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error = %v, want non-retryable", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("context cancellation did not interrupt socket I/O")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cancellation took %v", elapsed)
	}
	waitServer(t, server)
}

func TestPreparedCommitRetriesExactBytesAfterUnknownResult(t *testing.T) {
	t.Parallel()
	source := mustID(t, "00112233445566778899aabbccddeeff")
	prepared := prepareTestCommit(t, source, 73)
	listener := listenUnix(t)
	server := runServer(listener, func(first net.Conn) error {
		if err := serverHello(first, source); err != nil {
			return err
		}
		firstFrame, _, err := readRawFrame(first)
		if err != nil {
			return err
		}
		if err := first.Close(); err != nil {
			return err
		}

		second, err := listener.Accept()
		if err != nil {
			return err
		}
		defer second.Close()
		if err := serverHello(second, source); err != nil {
			return err
		}
		message, err := ReadMessage(second)
		if err != nil {
			return err
		}
		health, ok := message.(HealthRequest)
		if !ok {
			return fmt.Errorf("got %T, want HealthRequest", message)
		}
		if err := WriteMessage(second, HealthResponse{
			Nonce:    health.Nonce,
			SourceID: source,
			Status:   HealthHealthy,
		}); err != nil {
			return err
		}
		secondFrame, message, err := readRawFrame(second)
		if err != nil {
			return err
		}
		if !bytes.Equal(firstFrame, secondFrame) || !bytes.Equal(secondFrame, prepared.Bytes()) {
			return fmt.Errorf("retry changed prepared frame bytes")
		}
		batch, ok := message.(CommitBatchRequest)
		if !ok {
			return fmt.Errorf("got %T, want CommitBatchRequest", message)
		}
		accepted := batch.Sequence
		durable := batch.Sequence
		return WriteMessage(second, Ack{
			Kind:                    AckCommitBatch,
			SourceID:                source,
			Sequence:                batch.Sequence,
			CommitID:                batch.CommitID,
			AcceptedThroughSequence: &accepted,
			DurableThroughSequence:  &durable,
			Durable:                 true,
			Points:                  1,
		})
	})

	first := connectTestClient(t, listener.Addr().String(), source, time.Second)
	if _, err := first.CommitPrepared(context.Background(), prepared); err == nil || !ShouldRetry(err) {
		t.Fatalf("unknown first result error = %v, want retryable", err)
	}
	_ = first.Close()

	second := connectTestClient(t, listener.Addr().String(), source, time.Second)
	defer second.Close()
	health, err := second.Health(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if health.DurableThroughSequence != nil {
		t.Fatalf("durable watermark %#v, want nil", health.DurableThroughSequence)
	}
	ack, err := second.CommitPrepared(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Durable {
		t.Fatal("retry did not receive a durable ack")
	}
	waitServer(t, server)
}

func listenUnix(t *testing.T) *net.UnixListener {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "ftwdbshadow-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "shadow.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func connectTestClient(t *testing.T, path string, source ID128, timeout time.Duration) *Client {
	t.Helper()
	client, hello, err := Connect(context.Background(), ClientConfig{
		SocketPath:    path,
		SourceID:      source,
		NodeID:        "test-box",
		ClientVersion: "go-test",
		IOTimeout:     timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hello.SelectedVersion != ProtocolVersion {
		t.Fatalf("selected version %d", hello.SelectedVersion)
	}
	return client
}

func prepareTestCommit(t *testing.T, source ID128, sequence uint64) PreparedCommit {
	t.Helper()
	commit := mustID(t, "ffeeddccbbaa99887766554433221100")
	prepared, err := PrepareCommit(CommitBatchRequest{
		SourceID: source,
		Sequence: sequence,
		CommitID: commit,
		Points: []Point{{
			SeriesID: 1,
			Value:    -100,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func runServer(listener net.Listener, handler func(net.Conn) error) <-chan error {
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		result <- handler(conn)
	}()
	return result
}

func waitServer(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test sidecar did not stop")
	}
}

func serverHello(conn net.Conn, source ID128) error {
	message, err := ReadMessage(conn)
	if err != nil {
		return err
	}
	hello, ok := message.(HelloRequest)
	if !ok {
		return fmt.Errorf("got %T, want HelloRequest", message)
	}
	if hello.SourceID != source {
		return fmt.Errorf("hello source %s, want %s", hello.SourceID, source)
	}
	return WriteMessage(conn, HelloResponse{
		SelectedVersion: ProtocolVersion,
		SessionID:       source,
	})
}

func readRawFrame(reader io.Reader) ([]byte, Message, error) {
	header := make([]byte, headerBytes)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, nil, err
	}
	payload := int(binary.BigEndian.Uint32(header[8:12]))
	if payload > maxPayloadBytes {
		return nil, nil, fmt.Errorf("test received oversized payload %d", payload)
	}
	frame := make([]byte, headerBytes+payload+checksumBytes)
	copy(frame, header)
	if _, err := io.ReadFull(reader, frame[headerBytes:]); err != nil {
		return nil, nil, err
	}
	message, err := Decode(frame)
	return frame, message, err
}
