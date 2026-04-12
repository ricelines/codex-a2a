package service

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestAgentCardHandlerServesStableInterfaceShape(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.test/.well-known/agent-card.json", nil)
	rw := httptest.NewRecorder()

	AgentCardHandler(DefaultConfig()).ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Fatalf("status = %d, want 200", rw.Code)
	}

	var card map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}

	if _, ok := card["supportedInterfaces"]; !ok {
		t.Fatalf("supportedInterfaces missing from card: %s", rw.Body.String())
	}
	if _, ok := card["additionalInterfaces"]; ok {
		t.Fatalf("additionalInterfaces unexpectedly present: %s", rw.Body.String())
	}

	interfaces, ok := card["supportedInterfaces"].([]any)
	if !ok || len(interfaces) != 1 {
		t.Fatalf("supportedInterfaces = %#v, want one entry", card["supportedInterfaces"])
	}

	first, ok := interfaces[0].(map[string]any)
	if !ok {
		t.Fatalf("first interface has wrong shape: %#v", interfaces[0])
	}
	if first["protocolBinding"] != "JSONRPC" {
		t.Fatalf("protocolBinding = %#v, want JSONRPC", first["protocolBinding"])
	}
	if first["protocolVersion"] != string(a2a.Version) {
		t.Fatalf("protocolVersion = %#v, want %s", first["protocolVersion"], a2a.Version)
	}
}
