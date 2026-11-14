package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
)

const refusedHost = "staging.example.com"

// refusingBinary is a stand-in for vegaload that refuses refusedHost the way
// the real CLI does (exit 2 and a refused_hosts object for run, a result with
// refused_hosts for validate) unless it is given -allow-target refusedHost.
// Each invocation's arguments are appended to the returned file, one per
// line, with a "---" line after each invocation.
func refusingBinary(t *testing.T, okStdout string) (binPath, argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a /bin/sh script, which Windows cannot run")
	}
	dir := t.TempDir()
	binPath = filepath.Join(dir, "fake-vegaload")
	argsFile = filepath.Join(dir, "args.txt")
	runRefusal := fmt.Sprintf(`{"error":"refusing to run against \"https://%s/\" without confirmation","refused_hosts":["%s"]}`, refusedHost, refusedHost)
	validateRefusal := fmt.Sprintf(`{"scenario":"s.vl.js","valid":false,"stage":"iteration","error":"refused","hosts":[],"refused_hosts":["%s"]}`, refusedHost)
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" >> %s
echo --- >> %s
ok=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-allow-target" ] && [ "$a" = "%s" ]; then ok=1; fi
  prev="$a"
done
if [ -n "$ok" ]; then printf '%%s' %s; exit 0; fi
if [ "$1" = "validate" ]; then printf '%%s' %s; exit 1; fi
echo 'vegaload run: refusing' >&2
printf '%%s' %s
exit 2
`, shQuote(argsFile), shQuote(argsFile), refusedHost, shQuote(okStdout), shQuote(validateRefusal), shQuote(runRefusal))
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, argsFile
}

// invocations reads each call's arguments from argsFile.
func invocations(t *testing.T, argsFile string) [][]string {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "---" {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return out
}

// mcpClient is a fake MCP client talking to a Server over stdio.
type mcpClient struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Scanner
	done   chan error
	answer func(params map[string]any) map[string]any // nil: the client does not declare elicitation

	asked []string // the message of each elicitation/create
}

func newMCPClient(t *testing.T, srv *Server, answer func(map[string]any) map[string]any) *mcpClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &mcpClient{t: t, in: inW, out: bufio.NewScanner(outR), done: make(chan error, 1), answer: answer}
	c.out.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	go func() {
		c.done <- srv.Serve(context.Background(), inR, outW)
		outW.Close()
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after the client closed")
		}
	})
	caps := map[string]any{}
	if answer != nil {
		caps["elicitation"] = map[string]any{}
	}
	if resp := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": caps}); resp["result"] == nil {
		t.Fatalf("initialize: %v", resp)
	}
	return c
}

func (c *mcpClient) send(msg map[string]any) {
	c.t.Helper()
	msg["jsonrpc"] = "2.0"
	data, _ := json.Marshal(msg)
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.t.Fatalf("writing to the server: %v", err)
	}
}

// call sends a request and returns its response, answering any
// elicitation/create the server sends meanwhile.
func (c *mcpClient) call(method string, params any) map[string]any {
	c.t.Helper()
	id := fmt.Sprintf("c-%s-%d", method, time.Now().UnixNano())
	c.send(map[string]any{"id": id, "method": method, "params": params})
	for c.out.Scan() {
		var m map[string]any
		if err := json.Unmarshal(c.out.Bytes(), &m); err != nil {
			c.t.Fatalf("server sent %q: %v", c.out.Text(), err)
		}
		if m["method"] == "elicitation/create" {
			params, _ := m["params"].(map[string]any)
			msg, _ := params["message"].(string)
			c.asked = append(c.asked, msg)
			if c.answer == nil {
				c.t.Fatalf("the server asked a client that did not declare elicitation: %v", m)
			}
			c.send(map[string]any{"id": m["id"], "result": c.answer(params)})
			continue
		}
		if m["id"] == id {
			return m
		}
	}
	c.t.Fatalf("the server closed the stream before answering %s", method)
	return nil
}

// toolText returns a tools/call result's first text and whether it is an error.
func toolText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	content := res["content"].([]any)
	return content[0].(map[string]any)["text"].(string), res["isError"] == true
}

func serverWith(bin string) *Server {
	srv := NewServer()
	for _, tool := range NewTools(bin) {
		srv.Register(tool)
	}
	return srv
}

func accept(allow bool) func(map[string]any) map[string]any {
	return func(map[string]any) map[string]any {
		return map[string]any{"action": "accept", "content": map[string]any{"allow": allow}}
	}
}

var runTestCall = map[string]any{"name": "run_test", "arguments": map[string]any{
	"target": "https://" + refusedHost + "/", "protocol": "http1", "vus": 2, "duration": "5s", "no_report": true,
}}

// (i) A client that can ask, and a person who approves: the same command
// runs again with -allow-target for the host, and nothing else added.
func TestRunTest_ElicitationAcceptedRerunsWithAllowTarget(t *testing.T) {
	resultJSON, _ := json.Marshal(report.Result{Executor: "fixed-vus", Total: 7})
	bin, argsFile := refusingBinary(t, string(resultJSON))
	c := newMCPClient(t, serverWith(bin), accept(true))

	text, isErr := toolText(t, c.call("tools/call", runTestCall))
	if isErr {
		t.Fatalf("run_test failed: %s", text)
	}
	if !strings.Contains(text, `"total": 7`) {
		t.Fatalf("result %s", text)
	}
	if len(c.asked) != 1 || !strings.Contains(c.asked[0], refusedHost) || !strings.Contains(c.asked[0], "vegaload run") {
		t.Fatalf("asked %q", c.asked)
	}
	calls := invocations(t, argsFile)
	if len(calls) != 2 {
		t.Fatalf("invocations %v", calls)
	}
	want := append(append([]string(nil), calls[0]...), "-allow-target", refusedHost)
	if strings.Join(calls[1], " ") != strings.Join(want, " ") {
		t.Fatalf("second run %v, want the first plus -allow-target only: %v", calls[1], want)
	}
	for _, a := range calls[1] {
		if a == "-yes" {
			t.Fatal("-yes was passed")
		}
	}
}

// (ii) A person who declines, or accepts without ticking yes: an error, and
// no second run.
func TestRunTest_ElicitationDeclinedRunsNothing(t *testing.T) {
	answers := map[string]func(map[string]any) map[string]any{
		"decline":      func(map[string]any) map[string]any { return map[string]any{"action": "decline"} },
		"cancel":       func(map[string]any) map[string]any { return map[string]any{"action": "cancel"} },
		"accept-false": accept(false),
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			bin, argsFile := refusingBinary(t, "{}")
			c := newMCPClient(t, serverWith(bin), answer)
			text, isErr := toolText(t, c.call("tools/call", runTestCall))
			if !isErr || !strings.Contains(text, "did not approve "+refusedHost) {
				t.Fatalf("want a tool error, got %v %s", isErr, text)
			}
			if n := len(invocations(t, argsFile)); n != 1 {
				t.Fatalf("%d invocations, want 1", n)
			}
		})
	}
}

// (iii) A client without elicitation: an error with the command a person can
// run in a terminal, and nothing run again.
func TestRunTest_NoElicitationReturnsTheTerminalCommand(t *testing.T) {
	bin, argsFile := refusingBinary(t, "{}")
	c := newMCPClient(t, serverWith(bin), nil)
	text, isErr := toolText(t, c.call("tools/call", map[string]any{"name": "run_test", "arguments": map[string]any{
		"scenario_path": "my flow.vl.js", "vus": 2, "no_report": true, "thresholds": []string{"p95 < 300ms"},
	}}))
	if !isErr {
		t.Fatalf("want a tool error, got %s", text)
	}
	want := "vegaload run -vus 2 -threshold 'p95 < 300ms' -no-report -allow-target " + refusedHost + " 'my flow.vl.js'"
	if !strings.Contains(text, want) {
		t.Fatalf("error %q does not give the command %q", text, want)
	}
	if strings.Contains(text, "-trigger") || strings.Contains(text, "-output json") {
		t.Fatalf("the command keeps the tool's own flags: %s", text)
	}
	if n := len(invocations(t, argsFile)); n != 1 {
		t.Fatalf("%d invocations, want 1", n)
	}
}

// (iv) yes and allow_targets are refused before anything runs.
func TestRunTestAndValidate_RefuseApprovalArguments(t *testing.T) {
	bin, argsFile := refusingBinary(t, "{}")
	tools := NewTools(bin)
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"run_test", map[string]any{"target": "https://" + refusedHost, "protocol": "http1", "yes": true}},
		{"run_test", map[string]any{"target": "https://" + refusedHost, "protocol": "http1", "allow_targets": []string{refusedHost}}},
		{"run_test", map[string]any{"scenario_path": "s.vl.js", "yes": false}},
		{"validate_scenario", map[string]any{"scenario_path": "s.vl.js", "yes": true}},
		{"validate_scenario", map[string]any{"scenario_path": "s.vl.js", "allow_targets": []string{refusedHost}}},
	}
	for _, tc := range cases {
		_, err := callTool(t, tools, tc.tool, tc.args)
		if err == nil || !strings.Contains(err.Error(), "an agent cannot approve a host by itself") {
			t.Errorf("%s %v: err = %v", tc.tool, tc.args, err)
		}
	}
	if calls := invocations(t, argsFile); len(calls) != 0 {
		t.Fatalf("the CLI ran: %v", calls)
	}
	for _, tool := range tools {
		if tool.Name != "run_test" && tool.Name != "validate_scenario" {
			continue
		}
		props := tool.InputSchema["properties"].(map[string]any)
		for _, k := range approvalArgs {
			if _, ok := props[k]; ok {
				t.Errorf("%s still offers %q", tool.Name, k)
			}
		}
	}
}

// validate_scenario asks the same way, and returns validate's result.
func TestValidateScenario_ElicitationAccepted(t *testing.T) {
	bin, argsFile := refusingBinary(t, `{"scenario":"s.vl.js","valid":true,"hosts":["`+refusedHost+`"],"refused_hosts":[]}`)
	c := newMCPClient(t, serverWith(bin), accept(true))
	text, isErr := toolText(t, c.call("tools/call", map[string]any{"name": "validate_scenario", "arguments": map[string]any{"scenario_path": "s.vl.js"}}))
	if isErr || !strings.Contains(text, `"valid": true`) {
		t.Fatalf("validate_scenario: %v %s", isErr, text)
	}
	calls := invocations(t, argsFile)
	if len(calls) != 2 {
		t.Fatalf("invocations %v", calls)
	}
	last := calls[1]
	if got := strings.Join(last[len(last)-3:], " "); got != "-allow-target "+refusedHost+" s.vl.js" {
		t.Fatalf("second validate ends %q, want -allow-target before the scenario", got)
	}
}

// The SSE transport carries elicitation too: the request goes out on the
// stream and the answer comes back as a POST to /message.
func TestHTTP_SSEElicitation(t *testing.T) {
	resultJSON, _ := json.Marshal(report.Result{Executor: "fixed-vus", Total: 3})
	bin, argsFile := refusingBinary(t, string(resultJSON))
	ts := httptest.NewServer(serverWith(bin).HTTPHandler("", ""))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	_, endpoint := readEvent(t, rd)

	postJSON := func(v any) {
		data, _ := json.Marshal(v)
		r := post(t, ts.URL+endpoint, string(data), nil)
		r.Body.Close()
		if r.StatusCode != http.StatusAccepted {
			t.Fatalf("POST status %d", r.StatusCode)
		}
	}
	postJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"elicitation": map[string]any{}}}})
	if _, data := readEvent(t, rd); !strings.Contains(data, `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("initialize answer %s", data)
	}
	postJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": runTestCall})

	for {
		_, data := readEvent(t, rd)
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatal(err)
		}
		if m["method"] == "elicitation/create" {
			postJSON(map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"action": "accept", "content": map[string]any{"allow": true}}})
			continue
		}
		if m["id"] != float64(2) {
			t.Fatalf("unexpected message %s", data)
		}
		text, isErr := toolText(t, m)
		if isErr || !strings.Contains(text, `"total": 3`) {
			t.Fatalf("run_test over SSE: %v %s", isErr, text)
		}
		break
	}
	if n := len(invocations(t, argsFile)); n != 2 {
		t.Fatalf("%d invocations, want 2", n)
	}
}

// POST /mcp cannot send the client a request, so it falls back to the
// terminal command even when the client declares elicitation.
func TestHTTP_PostMCPFallsBackToTheTerminalCommand(t *testing.T) {
	bin, _ := refusingBinary(t, "{}")
	ts := httptest.NewServer(serverWith(bin).HTTPHandler("", ""))
	defer ts.Close()
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": runTestCall})
	resp := post(t, ts.URL+"/mcp", string(data), nil)
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	text, isErr := toolText(t, m)
	if !isErr || !strings.Contains(text, "-allow-target "+refusedHost) {
		t.Fatalf("want the terminal command, got %v %s", isErr, text)
	}
}

func TestNegotiateVersion(t *testing.T) {
	for in, want := range map[string]string{
		`{}`:                                  protocolVersion,
		`{"protocolVersion":"2024-11-05"}`:    "2024-11-05",
		`{"protocolVersion":"2025-06-18"}`:    "2025-06-18",
		`{"protocolVersion":"2099-01-01"}`:    supportedVersions[0],
		`{"protocolVersion":"not a date"}`:    supportedVersions[0],
		`{"capabilities":{"elicitation":{}}}`: protocolVersion,
	} {
		if got := negotiateVersion(json.RawMessage(in)); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestSessionElicitationCapability(t *testing.T) {
	for in, want := range map[string]bool{
		`{}`:                                  false,
		`{"capabilities":{}}`:                 false,
		`{"capabilities":{"elicitation":{}}}`: true,
		`{"capabilities":{"elicitation":{"form":{}}}}`:          true,
		`{"capabilities":{"elicitation":{"url":{}}}}`:           false,
		`{"capabilities":{"elicitation":{"form":{},"url":{}}}}`: true,
	} {
		s := newSession(func([]byte) error { return nil })
		s.initialized(json.RawMessage(in))
		if s.canElicit() != want {
			t.Errorf("%s: canElicit %v, want %v", in, s.canElicit(), want)
		}
	}
}
