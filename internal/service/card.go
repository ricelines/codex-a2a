package service

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/a2a"
)

func AgentCard(cfg Config, baseURL string) *a2a.AgentCard {
	invokeURL := strings.TrimRight(baseURL, "/") + "/invoke"
	return &a2a.AgentCard{
		Name:        cfg.AgentName,
		Description: cfg.AgentDescription,
		Version:     "0.1.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(invokeURL, a2a.TransportProtocolJSONRPC),
		},
		DefaultInputModes:  []string{"text/plain", "application/json"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
		Capabilities: a2a.AgentCapabilities{
			Streaming: true,
		},
		Skills: []a2a.AgentSkill{
			{
				ID:          "codex",
				Name:        "Codex Software Engineering",
				Description: "Runs Codex turns behind A2A tasks, including streaming output, approvals, and multi-turn thread context.",
				Tags:        []string{"coding", "software-engineering", "terminal", "review"},
				Examples: []string{
					"Inspect this repository and explain the failing tests.",
					"Implement the requested change and summarize the diff.",
				},
			},
		},
	}
}

func AgentCardHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		baseURL := cfg.BaseURL
		if baseURL == "" {
			scheme := "http"
			if req.TLS != nil {
				scheme = "https"
			}
			if forwarded := req.Header.Get("X-Forwarded-Proto"); forwarded != "" {
				scheme = strings.Split(forwarded, ",")[0]
			}
			baseURL = scheme + "://" + req.Host
		}
		card := AgentCard(cfg, baseURL)
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Access-Control-Allow-Origin", "*")
		_ = json.NewEncoder(rw).Encode(card)
	})
}
