package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionToolsReportState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := NewSession(ctx, SessionConfig{TTL: time.Hour, Intent: "test"})
	defer sess.End("test_cleanup")
	a := NewAudit()
	a.Append(AuditEvent{Tool: "ftw_api", OutcomeOK: true, OutcomeMsg: "ok"})

	lg := NewSessionLogTool(sess, a)
	out, err := lg.Handle(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	md := out.(map[string]any)["markdown"].(string)
	if !strings.Contains(md, "intent") || !strings.Contains(md, "ftw_api") {
		t.Fatalf("expected intent + tool in markdown:\n%s", md)
	}

	rm := NewSessionRemainingTool(sess)
	r, _ := rm.Handle(context.Background(), nil)
	if r.(map[string]any)["seconds"].(int) <= 0 {
		t.Fatal("expected positive remaining")
	}
}

func TestSessionEndTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := NewSession(ctx, SessionConfig{TTL: time.Hour})
	end := NewSessionEndTool(sess)
	if _, err := end.Handle(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("session did not end")
	}
}
