// Package mcp implements FR-MCP-01/02/03's agent-native layer: a
// hand-rolled JSON-RPC 2.0 server speaking the Model Context Protocol
// over stdio (no third-party SDK — see go.mod's dependency-pinning
// history for why this project prefers stdlib-only solutions). It knows
// nothing about scenarios, protocols, or the engine itself: every tool
// it registers is a thin wrapper that shells out to the compiled
// vegaload binary (see exec.go's RunCLI) and parses that command's own
// -output json result, per AGENTS.md's module-boundary rule — "the MCP
// layer must not call core-engine functions directly" and "every MCP
// tool maps to the same underlying CLI command a human can run
// directly."
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// protocolVersion is the MCP protocol version this server answers a client
// that names no version.
const protocolVersion = "2024-11-05"

// supportedVersions are the MCP versions this server speaks, newest first.
// A client that asks for one of them gets it back; one that asks for another
// gets the newest. They differ, for this server, only in what the client may
// declare: elicitation came in 2025-06-18.
var supportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// negotiateVersion picks the version to answer initialize with.
func negotiateVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.ProtocolVersion == "" {
		return protocolVersion
	}
	for _, v := range supportedVersions {
		if v == p.ProtocolVersion {
			return v
		}
	}
	return supportedVersions[0]
}

// Tool is one MCP tool: a name and JSON-schema-shaped description an
// MCP client (Cursor, Claude Code, any other MCP host) shows an LLM, and
// a Handler that does the actual work when the LLM calls it. Handler
// receives the call's "arguments" object exactly as the client sent it
// (unparsed, so each tool decides its own shape) and returns any value
// JSON-encodable as the tool's result, or an error — which the server
// reports back as a tool-level failure (isError: true), not a
// transport-level JSON-RPC error, since a bad or failing run is a
// perfectly normal thing for an agent to see and react to.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, arguments json.RawMessage) (any, error)
}

// Server dispatches JSON-RPC 2.0 requests read from a stdio transport
// to registered Tools. It has no fields beyond the tool registry: it
// holds no run state of its own, since every tool call is a fresh
// subprocess invocation of the vegaload binary (RunCLI) that does its
// own bookkeeping (audit log, report files) exactly as a human-run CLI
// command would.
type Server struct {
	tools  []Tool
	byName map[string]*Tool
}

// NewServer returns a Server with no tools registered; call Register
// for each tool before Serve.
func NewServer() *Server {
	return &Server{byName: map[string]*Tool{}}
}

// Register adds t to the server's tool set. Registering two tools with
// the same Name replaces the earlier one, which is useful for tests but
// not expected in normal use (cmd/vegaload/mcp.go registers each real
// tool name exactly once).
func (s *Server) Register(t Tool) {
	if _, exists := s.byName[t.Name]; !exists {
		s.tools = append(s.tools, t)
	} else {
		for i := range s.tools {
			if s.tools[i].Name == t.Name {
				s.tools[i] = t
			}
		}
	}
	tCopy := t
	s.byName[t.Name] = &tCopy
}

// rpcRequest and rpcResponse are JSON-RPC 2.0's wire shapes. ID is kept
// as json.RawMessage so it round-trips exactly as the client sent it
// (a string, a number, or absent for a notification) rather than being
// forced through a single Go type.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Standard JSON-RPC 2.0 error codes used below.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Serve reads newline-delimited JSON-RPC 2.0 messages from r and writes
// responses to w — MCP's stdio transport, which (unlike LSP) uses a bare
// newline as the message delimiter rather than Content-Length framing.
// It runs until r is exhausted (EOF, the normal way an MCP client ends
// the session by closing the subprocess's stdin) or ctx is done.
//
// Requests are handled one at a time, in order. Reading goes on while a
// request is handled, so that the client's answer to a request this server
// sent it (elicitation/create, see Confirm) reaches the tool that waits for
// it.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	var wmu sync.Mutex
	sess := newSession(func(data []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := w.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("mcp: writing message: %w", err)
		}
		return nil
	})
	defer sess.close()

	read := make(chan []byte)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(read)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // a report.Result embedded in a tool response can be large
		for scanner.Scan() {
			line := trimSpace(scanner.Bytes())
			if len(line) == 0 || sess.deliverIfReply(line) {
				continue
			}
			select {
			case read <- append([]byte(nil), line...):
			case <-done:
				return
			}
		}
		readErr <- scanner.Err()
		sess.close() // no answer can arrive any more
	}()
	// The queue never makes the reader wait, so an answer that comes after
	// another request is still read while a tool waits for it.
	lines := queueLines(read, done)

	ctx = withSession(ctx, sess)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return <-readErr
			}
			resp := s.handleLine(ctx, line)
			if resp == nil {
				continue // a notification: no response is sent, per JSON-RPC 2.0
			}
			data, err := json.Marshal(resp)
			if err != nil {
				return fmt.Errorf("mcp: encoding response: %w", err)
			}
			if err := sess.send(data); err != nil {
				return err
			}
		}
	}
}

// queueLines passes on what arrives on in, in order, holding as many lines as
// it must so that a send on in never waits for the reader of the result. The
// result is closed after in is closed and everything was passed on.
func queueLines(in <-chan []byte, done <-chan struct{}) <-chan []byte {
	out := make(chan []byte)
	go func() {
		defer close(out)
		var q [][]byte
		for in != nil || len(q) > 0 {
			var send chan<- []byte
			var next []byte
			if len(q) > 0 {
				send, next = out, q[0]
			}
			select {
			case line, ok := <-in:
				if !ok {
					in = nil
					continue
				}
				q = append(q, line)
			case send <- next:
				q = q[1:]
			case <-done:
				return
			}
		}
	}()
	return out
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// handleLine parses and dispatches one line. It returns nil for a
// notification (a request with no "id") — including when the method is
// one of this server's own fire-and-forget methods
// ("notifications/initialized") — since JSON-RPC 2.0 notifications never
// receive a response, success or failure.
func (s *Server) handleLine(ctx context.Context, line []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return &rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: codeParseError, Message: "parse error: " + err.Error()}}
	}
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	result, rpcErr := s.dispatch(ctx, req)
	if isNotification {
		return nil
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	return resp
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		if sess := sessionFrom(ctx); sess != nil {
			sess.initialized(req.Params)
		}
		return map[string]any{
			"protocolVersion": negotiateVersion(req.Params),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "vegaload", "version": "mcp-0"},
		}, nil
	case "notifications/initialized", "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.toolList()}, nil
	case "tools/call":
		return s.callTool(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

// toolDescriptor is what tools/list advertises per tool — name,
// description, and the JSON schema an MCP client uses both to validate
// arguments and to show the LLM what it can pass.
type toolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (s *Server) toolList() []toolDescriptor {
	out := make([]toolDescriptor, 0, len(s.tools))
	for _, t := range s.tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, toolDescriptor{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return out
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params: " + err.Error()}
	}
	tool, ok := s.byName[p.Name]
	if !ok {
		return toolErrorResult(fmt.Sprintf("unknown tool %q", p.Name)), nil
	}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	result, err := tool.Handler(ctx, args)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	notes := ""
	if wn, ok := result.(WithNotes); ok {
		result, notes = wn.Value, wn.Notes
	}
	text, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return toolErrorResult(fmt.Sprintf("encoding result: %v", err)), nil
	}
	// The first item is always the result, alone, so a client that parses it
	// is never handed a note. Notes come as a second item, only when there are some.
	content := []map[string]any{{"type": "text", "text": string(text)}}
	if notes != "" {
		content = append(content, map[string]any{"type": "text", "text": notes})
	}
	return map[string]any{"content": content}, nil
}

// WithNotes is a tool result that carries notes for the person using the
// agent: what the CLI wrote to stderr, such as `run`'s "live view: URL" line.
// The server sends Value as the first text item and Notes as a second one.
// Encoded as JSON (in tests and in `mcp eval`) it is just Value.
type WithNotes struct {
	Value any
	Notes string
}

// MarshalJSON encodes the value alone.
func (w WithNotes) MarshalJSON() ([]byte, error) { return json.Marshal(w.Value) }

// toolErrorResult builds an MCP CallToolResult with isError: true — a
// tool-level failure, reported to the LLM as normal content it can read
// and react to, rather than a JSON-RPC protocol error.
func toolErrorResult(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}
