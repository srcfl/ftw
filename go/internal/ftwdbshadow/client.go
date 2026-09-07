package ftwdbshadow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

type FailureKind string

const (
	FailureTransport FailureKind = "transport"
	FailureProtocol  FailureKind = "protocol"
	FailureContract  FailureKind = "contract"
	FailureClosed    FailureKind = "closed"
)

// ClientError classifies local failures without parsing text.
type ClientError struct {
	Operation string
	Kind      FailureKind
	CanRetry  bool
	Err       error
}

func (e *ClientError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("ftwdb shadow %s failed: %s", e.Operation, e.Kind)
	}
	return fmt.Sprintf("ftwdb shadow %s failed: %s: %v", e.Operation, e.Kind, e.Err)
}

func (e *ClientError) Unwrap() error { return e.Err }

func (e *ClientError) Retryable() bool { return e.CanRetry }

// RemoteError is the sidecar's stable error code and retry decision.
type RemoteError struct {
	Code     ErrorCode
	CanRetry bool
	Message  string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("ftwdb shadow sidecar error %d: %s", e.Code, e.Message)
}

func (e *RemoteError) Retryable() bool { return e.CanRetry }

// ShouldRetry returns the explicit decision carried by a client or sidecar
// error. Unknown errors are not retryable.
func ShouldRetry(err error) bool {
	var value interface{ Retryable() bool }
	return errors.As(err, &value) && value.Retryable()
}

type ClientConfig struct {
	SocketPath    string
	SourceID      ID128
	NodeID        string
	ClientVersion string
	Capabilities  uint64
	IOTimeout     time.Duration
}

func (config ClientConfig) validate() error {
	switch {
	case config.SocketPath == "":
		return fmt.Errorf("socket path is required")
	case config.SourceID.IsZero():
		return fmt.Errorf("source id is required")
	case config.NodeID == "":
		return fmt.Errorf("node id is required")
	case config.ClientVersion == "":
		return fmt.Errorf("client version is required")
	case config.IOTimeout <= 0:
		return fmt.Errorf("I/O timeout must be positive")
	default:
		return nil
	}
}

// Client owns one source-bound sidecar stream. Calls are serialized because
// each request has exactly one response.
type Client struct {
	mu        sync.Mutex
	conn      net.Conn
	sourceID  ID128
	ioTimeout time.Duration
	closed    bool
}

// Connect opens a Unix stream and completes HELLO under one absolute deadline.
func Connect(ctx context.Context, config ClientConfig) (*Client, HelloResponse, error) {
	if err := config.validate(); err != nil {
		return nil, HelloResponse{}, clientFailure("connect", FailureContract, false, err)
	}
	helloFrame, err := Encode(HelloRequest{
		SourceID:      config.SourceID,
		NodeID:        config.NodeID,
		ClientVersion: config.ClientVersion,
		Capabilities:  config.Capabilities,
	})
	if err != nil {
		return nil, HelloResponse{}, clientFailure("connect", FailureContract, false, err)
	}
	deadline, dialContext, cancel, err := operationDeadline(ctx, config.IOTimeout)
	if err != nil {
		return nil, HelloResponse{}, clientFailure("connect", FailureTransport, false, err)
	}
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(dialContext, "unix", config.SocketPath)
	if err != nil {
		return nil, HelloResponse{}, clientFailure("connect", FailureTransport, retryableContext(ctx), err)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, HelloResponse{}, clientFailure("hello", FailureTransport, true, err)
	}
	stopCancellation := interruptOnCancel(ctx, conn)
	response, err := exchangeEncodedFrame(conn, helloFrame)
	stopCancellation()
	if err != nil {
		_ = conn.Close()
		return nil, HelloResponse{}, classifyExchangeError("hello", ctx, err)
	}
	hello, ok := response.(HelloResponse)
	if !ok {
		_ = conn.Close()
		return nil, HelloResponse{}, unexpectedResponse("hello", response)
	}
	if hello.SelectedVersion != ProtocolVersion {
		_ = conn.Close()
		return nil, HelloResponse{}, clientFailure(
			"hello",
			FailureContract,
			false,
			fmt.Errorf("sidecar selected protocol %d, want %d", hello.SelectedVersion, ProtocolVersion),
		)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, HelloResponse{}, clientFailure("hello", FailureTransport, true, err)
	}
	return &Client{
		conn:      conn,
		sourceID:  config.SourceID,
		ioTimeout: config.IOTimeout,
	}, hello, nil
}

func (client *Client) SourceID() ID128 {
	return client.sourceID
}

// PreparedCommit keeps the exact bytes used as the retry and idempotency key.
// Call PrepareCommit once, then reuse the value until the sidecar accepts it.
type PreparedCommit struct {
	sourceID ID128
	sequence uint64
	commitID ID128
	frame    []byte
}

func PrepareCommit(batch CommitBatchRequest) (PreparedCommit, error) {
	frame, err := Encode(batch)
	if err != nil {
		return PreparedCommit{}, err
	}
	return PreparedCommit{
		sourceID: batch.SourceID,
		sequence: batch.Sequence,
		commitID: batch.CommitID,
		frame:    frame,
	}, nil
}

// PreparedCommitFromFrame validates and copies an already encoded commit.
func PreparedCommitFromFrame(frame []byte) (PreparedCommit, error) {
	message, err := Decode(frame)
	if err != nil {
		return PreparedCommit{}, err
	}
	batch, ok := message.(CommitBatchRequest)
	if !ok {
		return PreparedCommit{}, protocolError(ProtocolInvalidField, "frame", "is not a commit request")
	}
	return PreparedCommit{
		sourceID: batch.SourceID,
		sequence: batch.Sequence,
		commitID: batch.CommitID,
		frame:    append([]byte(nil), frame...),
	}, nil
}

func (prepared PreparedCommit) SourceID() ID128 { return prepared.sourceID }

func (prepared PreparedCommit) Sequence() uint64 { return prepared.sequence }

func (prepared PreparedCommit) CommitID() ID128 { return prepared.commitID }

func (prepared PreparedCommit) Bytes() []byte {
	return append([]byte(nil), prepared.frame...)
}

// Commit encodes and sends one batch. Use PrepareCommit and CommitPrepared
// when the caller may need an exact retry after an unknown transport result.
func (client *Client) Commit(ctx context.Context, batch CommitBatchRequest) (Ack, error) {
	prepared, err := PrepareCommit(batch)
	if err != nil {
		return Ack{}, clientFailure("commit", FailureProtocol, false, err)
	}
	return client.CommitPrepared(ctx, prepared)
}

func (client *Client) CommitPrepared(ctx context.Context, prepared PreparedCommit) (Ack, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	deadline, stopCancellation, err := client.beginLocked(ctx, "commit")
	if err != nil {
		return Ack{}, err
	}
	defer func() {
		stopCancellation()
		client.endLocked()
	}()
	return client.commitPreparedLocked(ctx, deadline, prepared)
}

// DurableCommitResult proves that the sidecar synced through Sequence.
type DurableCommitResult struct {
	Commit         Ack
	Flush          *Ack
	DurableThrough uint64
}

// CommitDurable sends exact prepared bytes and flushes only when the commit
// acknowledgement is not yet durable. The commit and optional flush share one
// absolute deadline.
func (client *Client) CommitDurable(ctx context.Context, prepared PreparedCommit) (DurableCommitResult, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	deadline, stopCancellation, err := client.beginLocked(ctx, "commit-durable")
	if err != nil {
		return DurableCommitResult{}, err
	}
	defer func() {
		stopCancellation()
		client.endLocked()
	}()

	commit, err := client.commitPreparedLocked(ctx, deadline, prepared)
	if err != nil {
		return DurableCommitResult{}, err
	}
	if watermarkAtLeast(commit.DurableThroughSequence, prepared.sequence) {
		return DurableCommitResult{
			Commit:         commit,
			DurableThrough: *commit.DurableThroughSequence,
		}, nil
	}
	flush, err := client.flushLocked(ctx, deadline, prepared.sequence)
	if err != nil {
		return DurableCommitResult{}, err
	}
	return DurableCommitResult{
		Commit:         commit,
		Flush:          &flush,
		DurableThrough: *flush.DurableThroughSequence,
	}, nil
}

func (client *Client) Flush(ctx context.Context, throughSequence uint64) (Ack, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	deadline, stopCancellation, err := client.beginLocked(ctx, "flush")
	if err != nil {
		return Ack{}, err
	}
	defer func() {
		stopCancellation()
		client.endLocked()
	}()
	return client.flushLocked(ctx, deadline, throughSequence)
}

func (client *Client) Health(ctx context.Context, nonce uint64) (HealthResponse, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	_, stopCancellation, err := client.beginLocked(ctx, "health")
	if err != nil {
		return HealthResponse{}, err
	}
	defer func() {
		stopCancellation()
		client.endLocked()
	}()

	response, err := exchangeFrame(client.conn, HealthRequest{Nonce: nonce})
	if err != nil {
		return HealthResponse{}, client.exchangeFailedLocked("health", ctx, err)
	}
	health, ok := response.(HealthResponse)
	if !ok {
		return HealthResponse{}, client.contractFailedLocked("health", response)
	}
	if health.Nonce != nonce {
		return HealthResponse{}, client.contractErrorLocked(
			"health",
			fmt.Errorf("nonce %d, want %d", health.Nonce, nonce),
		)
	}
	if health.SourceID != client.sourceID {
		return HealthResponse{}, client.contractErrorLocked(
			"health",
			fmt.Errorf("source %s, want %s", health.SourceID, client.sourceID),
		)
	}
	if err := validateWatermarks(health.AcceptedThroughSequence, health.DurableThroughSequence); err != nil {
		return HealthResponse{}, client.contractErrorLocked("health", err)
	}
	return health, nil
}

func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return nil
	}
	client.closed = true
	return client.conn.Close()
}

func (client *Client) commitPreparedLocked(
	ctx context.Context,
	deadline time.Time,
	prepared PreparedCommit,
) (Ack, error) {
	if prepared.sourceID != client.sourceID {
		return Ack{}, clientFailure(
			"commit",
			FailureContract,
			false,
			fmt.Errorf("prepared source %s, want %s", prepared.sourceID, client.sourceID),
		)
	}
	if len(prepared.frame) == 0 {
		return Ack{}, clientFailure("commit", FailureContract, false, fmt.Errorf("prepared frame is empty"))
	}
	if err := client.conn.SetDeadline(deadline); err != nil {
		return Ack{}, client.transportFailedLocked("commit", ctx, err)
	}
	response, err := exchangeEncodedFrame(client.conn, prepared.frame)
	if err != nil {
		return Ack{}, client.exchangeFailedLocked("commit", ctx, err)
	}
	ack, ok := response.(Ack)
	if !ok {
		return Ack{}, client.contractFailedLocked("commit", response)
	}
	if ack.Kind != AckCommitBatch {
		return Ack{}, client.contractErrorLocked("commit", fmt.Errorf("ack kind %d, want commit", ack.Kind))
	}
	if ack.SourceID != client.sourceID {
		return Ack{}, client.contractErrorLocked(
			"commit",
			fmt.Errorf("source %s, want %s", ack.SourceID, client.sourceID),
		)
	}
	if ack.Sequence != prepared.sequence {
		return Ack{}, client.contractErrorLocked(
			"commit",
			fmt.Errorf("sequence %d, want %d", ack.Sequence, prepared.sequence),
		)
	}
	if ack.CommitID != prepared.commitID {
		return Ack{}, client.contractErrorLocked(
			"commit",
			fmt.Errorf("commit id %s, want %s", ack.CommitID, prepared.commitID),
		)
	}
	if !watermarkAtLeast(ack.AcceptedThroughSequence, prepared.sequence) {
		return Ack{}, client.contractErrorLocked(
			"commit",
			fmt.Errorf("accepted watermark does not cover sequence %d", prepared.sequence),
		)
	}
	if err := validateWatermarks(ack.AcceptedThroughSequence, ack.DurableThroughSequence); err != nil {
		return Ack{}, client.contractErrorLocked("commit", err)
	}
	if ack.Durable && !watermarkAtLeast(ack.DurableThroughSequence, prepared.sequence) {
		return Ack{}, client.contractErrorLocked(
			"commit",
			fmt.Errorf("durable ack does not cover sequence %d", prepared.sequence),
		)
	}
	return ack, nil
}

func (client *Client) flushLocked(ctx context.Context, deadline time.Time, throughSequence uint64) (Ack, error) {
	if err := client.conn.SetDeadline(deadline); err != nil {
		return Ack{}, client.transportFailedLocked("flush", ctx, err)
	}
	response, err := exchangeFrame(client.conn, FlushRequest{
		SourceID:        client.sourceID,
		ThroughSequence: throughSequence,
	})
	if err != nil {
		return Ack{}, client.exchangeFailedLocked("flush", ctx, err)
	}
	ack, ok := response.(Ack)
	if !ok {
		return Ack{}, client.contractFailedLocked("flush", response)
	}
	if ack.Kind != AckFlush {
		return Ack{}, client.contractErrorLocked("flush", fmt.Errorf("ack kind %d, want flush", ack.Kind))
	}
	if ack.SourceID != client.sourceID {
		return Ack{}, client.contractErrorLocked(
			"flush",
			fmt.Errorf("source %s, want %s", ack.SourceID, client.sourceID),
		)
	}
	if ack.Sequence != throughSequence {
		return Ack{}, client.contractErrorLocked(
			"flush",
			fmt.Errorf("sequence %d, want %d", ack.Sequence, throughSequence),
		)
	}
	if !ack.CommitID.IsZero() {
		return Ack{}, client.contractErrorLocked("flush", fmt.Errorf("flush commit id is not zero"))
	}
	if err := validateWatermarks(ack.AcceptedThroughSequence, ack.DurableThroughSequence); err != nil {
		return Ack{}, client.contractErrorLocked("flush", err)
	}
	if !ack.Durable || !watermarkAtLeast(ack.DurableThroughSequence, throughSequence) {
		return Ack{}, client.contractErrorLocked(
			"flush",
			fmt.Errorf("durable watermark does not cover sequence %d", throughSequence),
		)
	}
	return ack, nil
}

func (client *Client) beginLocked(
	ctx context.Context,
	operation string,
) (time.Time, func(), error) {
	if client.closed {
		return time.Time{}, nil, clientFailure(operation, FailureClosed, false, net.ErrClosed)
	}
	deadline, _, cancel, err := operationDeadline(ctx, client.ioTimeout)
	if err != nil {
		return time.Time{}, nil, clientFailure(operation, FailureTransport, false, err)
	}
	cancel()
	if err := client.conn.SetDeadline(deadline); err != nil {
		return time.Time{}, nil, client.transportFailedLocked(operation, ctx, err)
	}
	return deadline, interruptOnCancel(ctx, client.conn), nil
}

func (client *Client) endLocked() {
	if client.closed {
		return
	}
	if err := client.conn.SetDeadline(time.Time{}); err != nil {
		client.closed = true
		_ = client.conn.Close()
	}
}

func (client *Client) exchangeFailedLocked(operation string, ctx context.Context, err error) error {
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote
	}
	var protocol *ProtocolError
	if errors.As(err, &protocol) && protocol.Kind != ProtocolIO && protocol.Kind != ProtocolTruncated {
		client.closed = true
		_ = client.conn.Close()
		return clientFailure(operation, FailureProtocol, false, err)
	}
	return client.transportFailedLocked(operation, ctx, err)
}

func (client *Client) transportFailedLocked(operation string, ctx context.Context, err error) error {
	client.closed = true
	_ = client.conn.Close()
	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	return clientFailure(operation, FailureTransport, retryableContext(ctx), err)
}

func (client *Client) contractFailedLocked(operation string, response Message) error {
	return client.contractErrorLocked(operation, fmt.Errorf("unexpected response %T", response))
}

func (client *Client) contractErrorLocked(operation string, err error) error {
	client.closed = true
	_ = client.conn.Close()
	return clientFailure(operation, FailureContract, false, err)
}

func clientFailure(operation string, kind FailureKind, retryable bool, err error) error {
	return &ClientError{
		Operation: operation,
		Kind:      kind,
		CanRetry:  retryable,
		Err:       err,
	}
}

func classifyExchangeError(operation string, ctx context.Context, err error) error {
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote
	}
	var protocol *ProtocolError
	if errors.As(err, &protocol) && protocol.Kind != ProtocolIO && protocol.Kind != ProtocolTruncated {
		return clientFailure(operation, FailureProtocol, false, err)
	}
	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	return clientFailure(operation, FailureTransport, retryableContext(ctx), err)
}

func unexpectedResponse(operation string, response Message) error {
	return clientFailure(
		operation,
		FailureContract,
		false,
		fmt.Errorf("unexpected response %T", response),
	)
}

func exchangeFrame(conn net.Conn, request Message) (Message, error) {
	frame, err := Encode(request)
	if err != nil {
		return nil, err
	}
	return exchangeEncodedFrame(conn, frame)
}

func exchangeEncodedFrame(conn net.Conn, frame []byte) (Message, error) {
	if err := writeAll(conn, frame); err != nil {
		return nil, &ProtocolError{Kind: ProtocolIO, Err: err}
	}
	response, err := ReadMessage(conn)
	if err != nil {
		return nil, err
	}
	if remote, ok := response.(ErrorResponse); ok {
		return nil, &RemoteError{
			Code:     remote.Code,
			CanRetry: remote.Retryable,
			Message:  remote.Message,
		}
	}
	return response, nil
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if written < 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func operationDeadline(
	ctx context.Context,
	timeout time.Duration,
) (time.Time, context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, nil, nil, err
	}
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if !deadline.After(time.Now()) {
		return time.Time{}, nil, nil, context.DeadlineExceeded
	}
	dialContext, cancel := context.WithDeadline(ctx, deadline)
	return deadline, dialContext, cancel, nil
}

func interruptOnCancel(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

func retryableContext(ctx context.Context) bool {
	return ctx.Err() == nil
}

func watermarkAtLeast(watermark *uint64, sequence uint64) bool {
	return watermark != nil && *watermark >= sequence
}

func validateWatermarks(accepted, durable *uint64) error {
	if accepted == nil || durable == nil {
		return nil
	}
	if *durable > *accepted {
		return fmt.Errorf("durable watermark %d exceeds accepted watermark %d", *durable, *accepted)
	}
	return nil
}
