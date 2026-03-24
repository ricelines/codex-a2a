package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
)

type mockResponsesHandler struct {
	counter uint64
	item    map[string]any
}

func NewMockResponsesHandler(responseText string, responseItemJSON string) (http.Handler, error) {
	item, err := mockResponsesItem(responseText, responseItemJSON)
	if err != nil {
		return nil, err
	}
	return &mockResponsesHandler{item: item}, nil
}

func mockResponsesItem(responseText string, responseItemJSON string) (map[string]any, error) {
	if responseItemJSON == "" {
		return map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": responseText,
			}},
		}, nil
	}

	var item map[string]any
	if err := json.Unmarshal([]byte(responseItemJSON), &item); err != nil {
		return nil, fmt.Errorf("decode mock responses item JSON: %w", err)
	}
	if len(item) == 0 {
		return nil, fmt.Errorf("mock responses item JSON must decode to a non-empty object")
	}
	if _, ok := item["type"].(string); !ok {
		return nil, fmt.Errorf("mock responses item JSON must contain a string type field")
	}
	return item, nil
}

func (h *mockResponsesHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/responses" || req.URL.RawQuery != "" {
		http.Error(rw, "forbidden", http.StatusForbidden)
		return
	}

	if _, err := io.Copy(io.Discard, req.Body); err != nil {
		http.Error(rw, fmt.Sprintf("read request body: %v", err), http.StatusBadRequest)
		return
	}

	item, err := cloneJSONMap(h.item)
	if err != nil {
		http.Error(rw, fmt.Sprintf("clone mock response item: %v", err), http.StatusInternalServerError)
		return
	}

	id := atomic.AddUint64(&h.counter, 1)
	responseID := fmt.Sprintf("resp-%d", id)
	if _, ok := item["id"]; !ok {
		item["id"] = fmt.Sprintf("item-%d", id)
	}

	itemJSON, err := json.Marshal(item)
	if err != nil {
		http.Error(rw, fmt.Sprintf("encode response item: %v", err), http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(rw, fmt.Sprintf(
		"event: response.created\n"+
			"data: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\n"+
			"event: response.output_item.done\n"+
			"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n"+
			"event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[%s]}}\n\n",
		responseID,
		itemJSON,
		responseID,
		itemJSON,
	))
}

func cloneJSONMap(input map[string]any) (map[string]any, error) {
	blob, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var cloned map[string]any
	if err := json.Unmarshal(blob, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}
