# codex-a2a

`codex-a2a` is an A2A 0.3.x server that fronts Codex through `codex app-server`.

The wrapper maps:

- A2A `contextId` -> one related family of Codex thread snapshots
- A2A `taskId` -> one long-lived Codex thread for that conversation or workflow
- repeated messages on the same non-terminal `taskId` -> additional Codex turns on the same thread
- A2A task state `input-required` -> either Codex approvals / MCP elicitations or a paused conversation waiting for the next user message

This repository intentionally uses `codex app-server`, not `codex mcp-server`. The app-server has the stable thread/turn/item surface needed to support streaming tasks, approvals, cancellation, and multi-turn context without inventing wrapper-only control flow.

## Repository layout

- `cmd/codex-a2a/`: server entrypoint
- `internal/service/`: Codex client, A2A executor, and tests

The upstream `A2A/`, `a2a-go/`, and `codex/` checkouts are not needed by the built server. They were used to ground the implementation and can be removed.

## Requirements

- Go `1.24.4` or newer
- A working Codex installation, or a built `codex-app-server` binary, when running outside Docker

The default runtime path is:

- launch `codex app-server --listen stdio://`

If you already built `codex-app-server` directly, the wrapper can launch that binary instead.

## Build

```bash
go mod download
go build ./cmd/codex-a2a
```

## Test

```bash
go mod download
go test ./...
```

The default test suite includes:

- in-process fake `codex app-server` tests for approvals, cancellation, A2A context branching, lifecycle, and protocol compliance
- deterministic real-CLI tests against the actual `codex` binary when one is available locally; these use a temp `CODEX_HOME` plus a local mock Responses server and do not spend tokens

The live smoke suite is opt-in:

```bash
CODEX_A2A_RUN_LIVE=1 go test ./internal/service -run TestLiveCodexSmoke
```

If `CODEX_A2A_LIVE_AUTH_JSON` is not set, that live test uses `~/.codex/auth.json`.

## CI and image publishing

GitHub Actions now includes:

- `ci`: runs `gofmt`, `go vet`, and `go test ./...` on `main` and on pull requests
- `docker`: builds the Docker image on `main` and on pull requests, and pushes to `ghcr.io/<repo-owner>/codex-a2a` on `main`

Both workflows use dependency and BuildKit caches, and cancel superseded runs on the same ref.

Image versioning is driven by `version-series.txt`.

Examples:

- `0.1.x` publishes `v0.1.0`, `v0.1.1`, ...
- `1.2.x` publishes `v1.2.0`, `v1.2.1`, ... and updates `v1.2` plus `v1`
- `1.0.0-alpha.x` publishes `v1.0.0-alpha.0`, `v1.0.0-alpha.1`, ... and updates `v1.0.0.alpha`

On each `main` push, CI checks GHCR for the next unused `x` value in the configured series, then publishes:

- `latest`
- the fully resolved version tag
- the appropriate floating semver tag or tags for that series

Pull requests build the image but do not publish tags.

## Run

Using the `codex` CLI:

```bash
go run ./cmd/codex-a2a \
  --listen 127.0.0.1:9001 \
  --default-cwd /absolute/path/to/workspace
```

Using a direct `codex-app-server` binary:

```bash
go run ./cmd/codex-a2a \
  --listen 127.0.0.1:9001 \
  --default-cwd /absolute/path/to/workspace \
  --codex-app-server-bin /absolute/path/to/codex-app-server
```

Key flags:

- `--mode`: `a2a`, `auth-proxy`, or `mock-responses`
- `--listen`: HTTP listen address for the A2A server
- `--base-url`: public base URL used in the Agent Card; optional for local use
- `--default-cwd`: default working directory for new Codex threads
- `--model`: Codex model forwarded to new threads; alias for `--default-model`
- `--model-reasoning-effort`: Codex reasoning effort forwarded to new threads
- `--developer-instructions`: Codex developer instructions forwarded to new threads
- `--default-model`: optional default model override
- `--default-approval-policy`: `untrusted`, `on-failure`, `on-request`, or `never`
- `--default-sandbox`: `read-only`, `workspace-write`, or `danger-full-access`
- `--dangerously-bypass-approvals-and-sandbox`: convenience alias for `--default-approval-policy never` plus `--default-sandbox danger-full-access`
- `--mcp-server-url`: repeatable MCP server URL forwarded as `mcp_servers.<index>.url`
- `--codex-cli`: path to the `codex` CLI
- `--codex-app-server-bin`: direct path to a `codex-app-server` binary
- `--mock-responses-text`: assistant text emitted by `--mode mock-responses` when no explicit item JSON is configured
- `--mock-responses-item-json`: full JSON object emitted as the single Responses API output item in `--mode mock-responses`

Once running:

- Agent Card: `http://127.0.0.1:9001/.well-known/agent-card.json`
- JSON-RPC endpoint: `http://127.0.0.1:9001/invoke`

The wrapper does not accept caller-supplied Codex session overrides through A2A
metadata. Working directory, model selection, approval policy, and sandbox mode
are server-owned defaults configured at startup.

## Amber manifests

This repo ships a few Amber manifests with distinct responsibilities:

- `amber.json5`: convenience root manifest for a real Codex-backed agent; composes `codex-auth-proxy` with `codex-a2a-runtime`
- `amber/codex-a2a-runtime.json5`: bare `codex-a2a` runtime that requires a `responses_api` slot
- `amber/codex-auth-proxy.json5`: Responses API provider backed by a real Codex `auth.json`
- `amber/mock-responses-api.json5`: deterministic mock Responses API provider
- `amber/mock-codex-a2a.json5`: convenience root manifest for a mock-backed agent; composes `mock-responses-api` with `codex-a2a-runtime`

The important boundary is that `codex-a2a-runtime` always consumes a `responses_api`
capability. Real auth and mock behavior are separate provider components rather than
alternate routing paths hidden inside the runtime config.

If you want a no-token smoke setup in Amber, use `amber/mock-codex-a2a.json5`. If you
want to assemble your own scenario, bind either `amber/codex-auth-proxy.json5` or
`amber/mock-responses-api.json5` into `amber/codex-a2a-runtime.json5`.

## Task behavior

- Starting a message with no `taskId` creates a new A2A task. If `contextId` is also empty, the wrapper creates a new Codex thread.
- Starting a message with a new `taskId` and an existing `contextId` creates a new Codex thread by forking from the referenced prior task, or from the sole unambiguous branch in that context.
- Continuing the same `taskId` while it is in `input-required` appends a new Codex turn onto that task's existing thread instead of forking.
- Successful Codex turns leave the A2A task in `input-required` so clients can keep appending follow-up messages to the same task.
- Parallel tasks in the same `contextId` are still supported. Each new task gets its own Codex thread snapshot so explicit branches remain immutable.
- If a context has multiple branches, clients should send `referenceTaskIds`. The wrapper fails ambiguous follow-ups rather than silently guessing the wrong branch.

The last point is deliberate: `a2a-go` allows only one active execution per task, so this wrapper waits for a task to pause back to `input-required` before accepting another same-task message. Follow-up chat turns use Codex `turn/start` on the existing thread; `turn/steer` is reserved for already-active turns.

## Approvals and MCP elicitation

When Codex pauses for approval or structured input, the wrapper:

1. emits a `pending:user-input` artifact
2. moves the task to `input-required`
3. waits for another A2A message on the same `taskId`

Reply formats:

- Command approval: text `accept`, `decline`, `cancel`, or a data part with `{"decision":"accept"}`
- File approval: text `accept`, `decline`, or a data part with `{"decision":"accept"}`
- MCP elicitation: a data part with `{"action":"accept","content":{...}}`, or text `decline` / `cancel`

## Minimal 0.3 JSON-RPC example

New task:

```bash
curl -s http://127.0.0.1:9001/invoke \
  -H 'content-type: application/json' \
  -d '{
    "jsonrpc":"2.0",
    "id":"1",
    "method":"message/send",
    "params":{
      "message":{
        "messageId":"msg-1",
        "role":"user",
        "parts":[{"kind":"text","text":"Inspect this repository and summarize the current changes."}]
      }
    }
  }'
```

Streaming:

```bash
curl -N http://127.0.0.1:9001/invoke \
  -H 'content-type: application/json' \
  -H 'accept: text/event-stream' \
  -d '{
    "jsonrpc":"2.0",
    "id":"1",
    "method":"message/stream",
    "params":{
      "message":{
        "messageId":"msg-1",
        "role":"user",
        "parts":[{"kind":"text","text":"Make the requested change and explain the diff."}]
      }
    }
  }'
```

## Docker

Build the image:

```bash
docker build -t codex-a2a .
```

The runtime image now includes both `codex-a2a` and the upstream Linux `codex` CLI, so the default container can launch `codex app-server` directly.

Useful build args:

- `CODEX_VERSION`: upstream Codex release tag, or `latest`
- `RUNTIME_IMAGE`: runtime base image; defaults to `debian:trixie-slim`
- `CODEX_UID` / `CODEX_GID`: uid/gid for the non-root `codex` user inside the image

Basic run:

```bash
docker run --rm -p 9001:9001 \
  -v /absolute/path/to/workspace:/workspace \
  codex-a2a \
  --listen 0.0.0.0:9001 \
  --default-cwd /workspace
```

To reuse host Codex auth/config, bind-mount them read-only at `/home/codex/.codex`. The entrypoint copies supported files into an internal writable `CODEX_HOME`, so host files with restrictive permissions such as `0600` still work.

Supported copied files:

- `auth.json`
- `config.toml`
- `managed_config.toml`
- `.credentials.json`

Mount only `auth.json`:

```bash
docker run --rm -p 9001:9001 \
  -v /absolute/path/to/workspace:/workspace \
  -v "$HOME/.codex/auth.json":/home/codex/.codex/auth.json:ro \
  codex-a2a \
  --listen 0.0.0.0:9001 \
  --default-cwd /workspace
```

Mount the whole Codex config directory:

```bash
docker run --rm -p 9001:9001 \
  -v /absolute/path/to/workspace:/workspace \
  -v "$HOME/.codex":/home/codex/.codex:ro \
  codex-a2a \
  --listen 0.0.0.0:9001 \
  --default-cwd /workspace
```

If you need a different mount location, set `CODEX_SOURCE_HOME` to the directory that should be copied into the internal runtime home.
