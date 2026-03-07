package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/nhynes/codex-a2a/internal/service"
)

func main() {
	cfg := service.DefaultConfig()

	listenAddr := flag.String("listen", ":9001", "TCP listen address")
	baseURL := flag.String("base-url", "", "Public base URL used in the agent card")
	agentName := flag.String("agent-name", cfg.AgentName, "Agent card name")
	agentDescription := flag.String("agent-description", cfg.AgentDescription, "Agent card description")
	defaultCwd := flag.String("default-cwd", cfg.DefaultCwd, "Default working directory for new Codex threads")
	defaultModel := flag.String("default-model", "", "Default Codex model override")
	defaultApprovalPolicy := flag.String("default-approval-policy", cfg.DefaultApprovalPolicy, "Default Codex approval policy")
	defaultSandbox := flag.String("default-sandbox", cfg.DefaultSandbox, "Default Codex sandbox mode")
	codexCLI := flag.String("codex-cli", cfg.CodexCLI, "Path to the codex CLI used as `codex app-server --listen stdio://`")
	codexAppServerBin := flag.String("codex-app-server-bin", "", "Optional direct path to a codex-app-server binary")
	codexClientName := flag.String("codex-client-name", cfg.CodexClientName, "clientInfo.name sent to codex app-server")
	codexClientTitle := flag.String("codex-client-title", cfg.CodexClientTitle, "clientInfo.title sent to codex app-server")
	codexClientVersion := flag.String("codex-client-version", cfg.CodexClientVer, "clientInfo.version sent to codex app-server")
	flag.Parse()

	cfg.BaseURL = *baseURL
	cfg.AgentName = *agentName
	cfg.AgentDescription = *agentDescription
	cfg.DefaultCwd = *defaultCwd
	cfg.DefaultModel = *defaultModel
	cfg.DefaultApprovalPolicy = *defaultApprovalPolicy
	cfg.DefaultSandbox = *defaultSandbox
	cfg.CodexCLI = *codexCLI
	cfg.CodexAppServerBin = *codexAppServerBin
	cfg.CodexClientName = *codexClientName
	cfg.CodexClientTitle = *codexClientTitle
	cfg.CodexClientVer = *codexClientVersion

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
