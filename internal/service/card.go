package service

import (
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
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
		a2asrv.NewStaticAgentCardHandler(AgentCard(cfg, baseURL)).ServeHTTP(rw, req)
	})
}
