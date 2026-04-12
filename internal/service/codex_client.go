package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

type incomingMessage struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

type responseMessage struct {
	Result json.RawMessage
	Error  *rpcError
}

type codexClient struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	nextID int64

	mu      sync.Mutex
	pending map[string]chan responseMessage

	events chan incomingMessage
	done   chan error

	stderrMu   sync.Mutex
	stderrTail []string

	closed atomic.Bool
}

const stderrTailLines = 20

func launchCodexClient(ctx context.Context, cfg Config) (*codexClient, error) {
	procCtx := context.WithoutCancel(ctx)

	var command *exec.Cmd
	switch {
	case cfg.CodexAppServerBin != "":
		args := cfg.CodexArgs
		if len(args) == 0 {
			args = []string{"--listen", "stdio://"}
		}
		command = exec.CommandContext(procCtx, cfg.CodexAppServerBin, args...)
	default:
		args := append([]string{"app-server", "--listen", "stdio://"}, cfg.CodexArgs...)
		command = exec.CommandContext(procCtx, cfg.CodexCLI, args...)
	}
	command.Env = append(command.Environ(), splitEnv(cfg.ChildEnv)...)

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open codex stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open codex stdout: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("open codex stderr: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}

	client := &codexClient{
		cmd:     command,
		stdin:   stdin,
		pending: make(map[string]chan responseMessage),
		events:  make(chan incomingMessage, 256),
		done:    make(chan error, 1),
	}
	go client.readLoop(stdout)
	go client.stderrLoop(stderr)
	go client.waitLoop()

	if err := client.initialize(ctx, cfg); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

func (c *codexClient) initialize(ctx context.Context, cfg Config) error {
	var params initializeParams
	params.ClientInfo.Name = cfg.CodexClientName
	params.ClientInfo.Version = cfg.CodexClientVer
	if cfg.CodexClientTitle != "" {
		params.ClientInfo.Title = &cfg.CodexClientTitle
	}
	params.Capabilities = &struct {
		ExperimentalAPI bool `json:"experimentalApi"`
	}{
		ExperimentalAPI: true,
	}
	if _, err := c.request(ctx, "initialize", params); err != nil {
		return fmt.Errorf("initialize codex app-server: %w", err)
	}
	if err := c.notify(ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("ack initialize: %w", err)
	}
	return nil
}

func (c *codexClient) readLoop(stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			c.finish(fmt.Errorf("read codex stream: %w", err))
			return
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var env rpcEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			c.finish(fmt.Errorf("decode codex message %q: %w", string(line), err))
			return
		}

		if env.Method != "" {
			select {
			case c.events <- incomingMessage{ID: env.ID, Method: env.Method, Params: env.Params}:
			case <-c.done:
				return
			}
			continue
		}

		if len(env.ID) == 0 {
			continue
		}

		key := string(env.ID)

		c.mu.Lock()
		ch := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()

		if ch == nil {
			continue
		}

		ch <- responseMessage{Result: env.Result, Error: env.Error}
		close(ch)
	}
}

func (c *codexClient) waitLoop() {
	err := c.cmd.Wait()
	if c.closed.Load() {
		return
	}

	if err == nil {
		err = fmt.Errorf("codex app-server exited before the protocol completed")
	} else {
		err = fmt.Errorf("codex app-server exited: %w", err)
	}
	if summary := c.stderrSummary(); summary != "" {
		err = fmt.Errorf("%w\nstderr:\n%s", err, summary)
	}
	c.finish(err)
}

func (c *codexClient) finish(err error) {
	if c.closed.CompareAndSwap(false, true) {
		message := "codex app-server closed"
		if err != nil {
			message = err.Error()
		}
		c.mu.Lock()
		for key, ch := range c.pending {
			delete(c.pending, key)
			ch <- responseMessage{Error: &rpcError{Message: message}}
			close(ch)
		}
		c.mu.Unlock()
		close(c.events)
		c.done <- err
		close(c.done)
	}
}

func (c *codexClient) stderrLoop(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		c.appendStderr(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		c.appendStderr("stderr read error: " + err.Error())
	}
}

func (c *codexClient) appendStderr(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()

	c.stderrTail = append(c.stderrTail, line)
	if len(c.stderrTail) > stderrTailLines {
		c.stderrTail = append([]string(nil), c.stderrTail[len(c.stderrTail)-stderrTailLines:]...)
	}
}

func (c *codexClient) stderrSummary() string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	return strings.Join(c.stderrTail, "\n")
}

func (c *codexClient) Alive() bool {
	return !c.closed.Load()
}

func (c *codexClient) Close() error {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	return nil
}

func (c *codexClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	idKey := fmt.Sprintf("%d", id)

	req := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		req["params"] = params
	}

	ch := make(chan responseMessage, 1)

	c.mu.Lock()
	c.pending[idKey] = ch
	c.mu.Unlock()

	if err := c.writeJSON(req); err != nil {
		c.mu.Lock()
		delete(c.pending, idKey)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("codex %s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	case err := <-c.done:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *codexClient) notify(ctx context.Context, method string, params any) error {
	req := map[string]any{"method": method}
	if params != nil {
		req["params"] = params
	}
	return c.writeJSON(req)
}

func (c *codexClient) respond(ctx context.Context, id json.RawMessage, result any) error {
	req := map[string]any{"id": json.RawMessage(id), "result": result}
	return c.writeJSON(req)
}

func (c *codexClient) writeJSON(payload any) error {
	if !c.Alive() {
		return fmt.Errorf("codex app-server is not running")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal jsonrpc payload: %w", err)
	}
	data = append(data, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("write codex request: %w", err)
	}
	return nil
}

func (c *codexClient) next(ctx context.Context) (incomingMessage, error) {
	select {
	case msg, ok := <-c.events:
		if !ok {
			select {
			case err := <-c.done:
				if err == nil {
					err = fmt.Errorf("codex app-server closed")
				}
				return incomingMessage{}, err
			default:
				return incomingMessage{}, fmt.Errorf("codex app-server closed")
			}
		}
		return msg, nil
	case err := <-c.done:
		if err == nil {
			err = fmt.Errorf("codex app-server closed")
		}
		return incomingMessage{}, err
	case <-ctx.Done():
		return incomingMessage{}, ctx.Err()
	}
}

func decodeJSON[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	return v, nil
}

func formatRawJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err == nil {
		return out.String()
	}
	return strings.TrimSpace(string(raw))
}
