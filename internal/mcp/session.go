package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
)

// ErrNoElicitation is what Confirm returns when the client cannot ask the
// person a question: it did not declare the elicitation capability at
// initialize, or the transport has no way to send it a request (POST /mcp).
var ErrNoElicitation = errors.New("the MCP client cannot ask the user a question (it does not support elicitation)")

// session is one connected MCP client, for a transport that can send it
// messages: what it declared at initialize, and the requests this server sent
// it that wait for an answer. Tool handlers reach it through their context.
type session struct {
	send func([]byte) error

	mu          sync.Mutex
	elicitation bool
	nextID      int64
	pending     map[string]chan clientReply
	closed      bool
}

// clientReply is the client's answer to a request this server sent.
type clientReply struct {
	result json.RawMessage
	err    *rpcError
}

func newSession(send func([]byte) error) *session {
	return &session{send: send, pending: map[string]chan clientReply{}}
}

type sessionKey struct{}

func withSession(ctx context.Context, s *session) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, s)
}

func sessionFrom(ctx context.Context) *session {
	s, _ := ctx.Value(sessionKey{}).(*session)
	return s
}

// initialized records what the client declared in its initialize params. Form
// elicitation is supported when the capability is present and either empty
// or names "form" (an empty object means form in every MCP version).
func (s *session) initialized(params json.RawMessage) {
	var p struct {
		Capabilities struct {
			Elicitation *map[string]json.RawMessage `json:"elicitation"`
		} `json:"capabilities"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	can := false
	if e := p.Capabilities.Elicitation; e != nil {
		_, form := (*e)["form"]
		can = len(*e) == 0 || form
	}
	s.mu.Lock()
	s.elicitation = can
	s.mu.Unlock()
}

func (s *session) canElicit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.elicitation && !s.closed
}

// request sends method to the client and waits for its answer.
func (s *session) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("the MCP client disconnected")
	}
	s.nextID++
	id := "vegaload-" + strconv.FormatInt(s.nextID, 10)
	ch := make(chan clientReply, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("mcp: encoding %s: %w", method, err)
	}
	if err := s.send(data); err != nil {
		return nil, fmt.Errorf("mcp: sending %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r, ok := <-ch:
		if !ok {
			return nil, errors.New("the MCP client disconnected")
		}
		if r.err != nil {
			return nil, fmt.Errorf("the MCP client answered %s with an error: %s", method, r.err.Message)
		}
		return r.result, nil
	}
}

// deliverIfReply hands line to the request waiting for it when line is a
// JSON-RPC response (an id, a result or an error, and no method). It reports
// whether line was a response; a response to no pending request is dropped.
func (s *session) deliverIfReply(line []byte) bool {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method *string         `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(line, &m); err != nil || m.Method != nil || len(m.ID) == 0 || (m.Result == nil && m.Error == nil) {
		return false
	}
	var id string
	if err := json.Unmarshal(m.ID, &id); err != nil {
		return true
	}
	s.mu.Lock()
	ch := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if ch != nil {
		ch <- clientReply{result: m.Result, err: m.Error}
	}
	return true
}

// close fails every request still waiting: no answer can come any more.
func (s *session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
}

// Confirm asks the person using the MCP client a yes-or-no question, with
// elicitation/create, and waits for the answer. It returns true only when
// the person accepted and ticked yes. It returns ErrNoElicitation when the
// client cannot be asked; the caller then tells the agent what the person
// must do instead.
func Confirm(ctx context.Context, message string) (bool, error) {
	s := sessionFrom(ctx)
	if s == nil || !s.canElicit() {
		return false, ErrNoElicitation
	}
	raw, err := s.request(ctx, "elicitation/create", map[string]any{
		"message": message,
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"allow": map[string]any{
					"type":        "boolean",
					"title":       "Allow",
					"description": "Yes lets this one run send load to the host.",
					"default":     false,
				},
			},
			"required": []string{"allow"},
		},
	})
	if err != nil {
		return false, err
	}
	var answer struct {
		Action  string `json:"action"`
		Content struct {
			Allow bool `json:"allow"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return false, fmt.Errorf("the MCP client's answer to elicitation/create is not valid: %w", err)
	}
	return answer.Action == "accept" && answer.Content.Allow, nil
}
