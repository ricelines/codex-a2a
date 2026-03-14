package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/nhynes/codex-a2a/internal/service"
)

func main() {
	cfg := service.DefaultConfig()

	listenAddr := flag.String("listen", "127.0.0.1:9001", "TCP listen address")
	baseURL := flag.String("base-url", "", "Public base URL used in the agent card")
	agentName := flag.String("agent-name", cfg.AgentName, "Agent card name")
	agentDescription := flag.String("agent-description", cfg.AgentDescription, "Agent card description")
	defaultCwd := flag.String("default-cwd", cfg.DefaultCwd, "Default working directory for new Codex threads")
	var defaultModel flagString
	defaultModel.value = cfg.DefaultModel
	flag.Var(&defaultModel, "default-model", "Default Codex model override")
	var model flagString
	flag.Var(&model, "model", "Codex model forwarded to new threads")
	defaultApprovalPolicy := flag.String("default-approval-policy", cfg.DefaultApprovalPolicy, "Default Codex approval policy")
	defaultSandbox := flag.String("default-sandbox", cfg.DefaultSandbox, "Default Codex sandbox mode")
	dangerouslyBypassApprovalsAndSandbox := flag.Bool("dangerously-bypass-approvals-and-sandbox", false, "Skip approvals and use danger-full-access for all new Codex threads")
	modelReasoningEffort := flag.String("model-reasoning-effort", "", "Codex model reasoning effort forwarded to new threads")
	var developerInstructions flagString
	flag.Var(&developerInstructions, "developer-instructions", "Codex developer instructions forwarded to new threads")
	codexCLI := flag.String("codex-cli", cfg.CodexCLI, "Path to the codex CLI used as `codex app-server --listen stdio://`")
	codexAppServerBin := flag.String("codex-app-server-bin", "", "Optional direct path to a codex-app-server binary")
	codexClientName := flag.String("codex-client-name", cfg.CodexClientName, "clientInfo.name sent to codex app-server")
	codexClientTitle := flag.String("codex-client-title", cfg.CodexClientTitle, "clientInfo.title sent to codex app-server")
	codexClientVersion := flag.String("codex-client-version", cfg.CodexClientVer, "clientInfo.version sent to codex app-server")
	var mcpServerURLs multiStringFlag
	flag.Var(&mcpServerURLs, "mcp-server-url", "Register an MCP server URL with an auto-generated numeric id; repeatable")
	flag.Parse()

	cfg.BaseURL = *baseURL
	cfg.AgentName = *agentName
	cfg.AgentDescription = *agentDescription
	cfg.DefaultCwd = *defaultCwd
	switch {
	case model.set:
		cfg.DefaultModel = model.value
	case defaultModel.set:
		cfg.DefaultModel = defaultModel.value
	}
	cfg.DefaultApprovalPolicy = *defaultApprovalPolicy
	cfg.DefaultSandbox = *defaultSandbox
	if *dangerouslyBypassApprovalsAndSandbox {
		cfg.DefaultApprovalPolicy = "never"
		cfg.DefaultSandbox = "danger-full-access"
	}
	cfg.CodexCLI = *codexCLI
	cfg.CodexAppServerBin = *codexAppServerBin
	cfg.CodexClientName = *codexClientName
	cfg.CodexClientTitle = *codexClientTitle
	cfg.CodexClientVer = *codexClientVersion
	cfg.MCPServerURLs = append([]string(nil), mcpServerURLs...)
	if *modelReasoningEffort != "" {
		cfg.CodexConfig["model_reasoning_effort"] = *modelReasoningEffort
	}
	if developerInstructions.set {
		cfg.CodexConfig["developer_instructions"] = developerInstructions.value
	}

	executor, err := service.NewExecutor(cfg)
	if err != nil {
		log.Fatalf("configure executor: %v", err)
	}
	defer func() {
		if err := executor.Close(); err != nil {
			log.Printf("close executor: %v", err)
		}
	}()

	card := service.AgentCard(cfg, cardBaseURL(cfg, *listenAddr))
	handler := service.NewHandler(executor)

	mux := http.NewServeMux()
	mux.Handle("/invoke", a2asrv.NewJSONRPCHandler(handler))
	mux.Handle(a2asrv.WellKnownAgentCardPath, service.AgentCardHandler(cfg))
	mux.Handle("/", http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(rw, req)
			return
		}
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = rw.Write([]byte(card.Name + "\n"))
	}))

	server := &http.Server{Addr: *listenAddr, Handler: mux}
	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	go func() {
		log.Printf("listening on %s", *listenAddr)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
}

func cardBaseURL(cfg service.Config, listenAddr string) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	if listenAddr == "" {
		return "http://127.0.0.1:9001"
	}
	if listenAddr[0] == ':' {
		return "http://127.0.0.1" + listenAddr
	}
	return "http://" + listenAddr
}

type flagString struct {
	value string
	set   bool
}

func (f *flagString) String() string {
	return f.value
}

func (f *flagString) Set(value string) error {
	f.value = value
	f.set = true
	return nil
}

type multiStringFlag []string

func (f *multiStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *multiStringFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}
