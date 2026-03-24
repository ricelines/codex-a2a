package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewMockResponsesHandlerDefaultsToAssistantText(t *testing.T) {
	handler, err := NewMockResponsesHandler("hello", "")
	if err != nil {
		t.Fatalf("NewMockResponsesHandler: %v", err)
	}

	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("POST /v1/responses: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if !strings.Contains(string(body), `"text":"hello"`) {
		t.Fatalf("response body = %s, want hello output text", body)
	}
}

func TestNewMockResponsesHandlerUsesCustomItemJSON(t *testing.T) {
	handler, err := NewMockResponsesHandler("", `{"type":"function_call","call_id":"call-1","name":"tool","arguments":"{}","status":"completed"}`)
	if err != nil {
		t.Fatalf("NewMockResponsesHandler: %v", err)
	}

	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"input":"hi"}`))
	if err != nil {
		t.Fatalf("POST /v1/responses: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if !strings.Contains(string(body), `"type":"function_call"`) {
		t.Fatalf("response body = %s, want function_call item", body)
	}
	if !strings.Contains(string(body), `"call_id":"call-1"`) {
		t.Fatalf("response body = %s, want call_id", body)
	}
}

func TestNewMockResponsesHandlerRejectsInvalidItemJSON(t *testing.T) {
	if _, err := NewMockResponsesHandler("", `[]`); err == nil {
		t.Fatal("expected invalid item JSON to fail")
	}
	if _, err := NewMockResponsesHandler("", `{"id":"x"}`); err == nil {
		t.Fatal("expected missing type to fail")
	}
}

func TestCloneJSONMapRoundTripsNestedValues(t *testing.T) {
	cloned, err := cloneJSONMap(map[string]any{
		"type": "message",
		"content": []any{
			map[string]any{
				"type": "output_text",
				"text": "hello",
			},
		},
	})
	if err != nil {
		t.Fatalf("cloneJSONMap: %v", err)
	}
	blob, err := json.Marshal(cloned)
	if err != nil {
		t.Fatalf("marshal cloned map: %v", err)
	}
	if !strings.Contains(string(blob), `"hello"`) {
		t.Fatalf("cloned blob = %s, want nested content", blob)
	}
}
