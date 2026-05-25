package main

import (
	"strings"
	"testing"
)

func TestAuditAppendAndRender(t *testing.T) {
	a := NewAudit()
	a.Append(AuditEvent{Tool: "ftw_api", Args: map[string]any{"path": "/api/status"}, OutcomeOK: true, OutcomeMsg: "200 OK, 432 bytes"})
	a.Append(AuditEvent{Tool: "deploy_driver", Args: map[string]any{"name": "goodwe_xs"}, OutcomeOK: false, OutcomeMsg: "config parse error"})

	md := a.RenderMarkdown()
	if !strings.Contains(md, "ftw_api") || !strings.Contains(md, "deploy_driver") {
		t.Fatalf("expected both tools in markdown, got:\n%s", md)
	}
	if !strings.Contains(md, "200 OK") {
		t.Fatalf("expected outcome string in markdown:\n%s", md)
	}
}

func TestAuditFileDiffTracking(t *testing.T) {
	a := NewAudit()
	a.RecordFileWrite("drivers/goodwe_xs.lua", "OLD", "NEW")
	md := a.RenderMarkdown()
	if !strings.Contains(md, "goodwe_xs.lua") {
		t.Fatalf("expected file path in diff section:\n%s", md)
	}
	if !strings.Contains(md, "-OLD") || !strings.Contains(md, "+NEW") {
		t.Fatalf("expected unified diff in markdown:\n%s", md)
	}
}
