package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vegaload/vegaload/internal/report"
	"github.com/vegaload/vegaload/internal/stream"
)

// The exit codes in website/docs/reference/cli.md ("Exit codes"), pinned
// command by command through the same entry point main uses. A change here
// is a change to that table, and the other way round.
func TestExitCodes(t *testing.T) {
	srv := okServer(t)
	dir := t.TempDir()
	audit := filepath.Join(dir, "audit.log")
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	okScenario := write("ok.vl.js", `export default function () { http.get("`+srv.URL+`/"); }`)
	brokenScenario := write("broken.vl.js", "export default function () { const = ; }")
	refusedScenario := write("refused.vl.js", `export default function () { http.get("http://staging.example.com/"); }`)
	baseline := filepath.Join(dir, "baseline.json")
	worse := filepath.Join(dir, "worse.json")
	if err := report.WriteJSON(baseline, &report.Result{Executor: "fixed-vus", Total: 100, Failed: 0, Latency: report.Latency{P95: time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	if err := report.WriteJSON(worse, &report.Result{Executor: "fixed-vus", Total: 100, Failed: 50, ErrorRate: 0.5, Latency: report.Latency{P95: time.Second}}); err != nil {
		t.Fatal(err)
	}
	// Paced, so that no test opens sockets in a free-running loop.
	runFlags := []string{"run", "-no-report", "-no-live", "-audit-log", audit, "-executor", "constant-arrival-rate", "-rate", "20", "-max-vus", "2", "-duration", "200ms"}
	runWith := func(extra ...string) []string { return append(append([]string(nil), runFlags...), extra...) }

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, 1},
		{"unknown command", []string{"bogus"}, 1},
		{"help", []string{"help"}, 0},
		{"version", []string{"version"}, 0},
		{"version json", []string{"version", "-output", "json"}, 0},
		{"version bad output", []string{"version", "-output", "yaml"}, 2},

		{"run help", []string{"run", "-h"}, 0},
		{"run bad flag", []string{"run", "-no-such-flag"}, 2},
		{"run nothing to run", []string{"run"}, 2},
		{"run passes", runWith(okScenario), 0},
		{"run scenario cannot load", runWith(brokenScenario), 1},
		{"run threshold breached", runWith("-threshold", "p95 < 1ns", okScenario), 3},
		{"run target refused", runWith("-target", "https://staging.example.com/", "-protocol", "http1"), 2},
		{"run scenario host refused", runWith(refusedScenario), 2},

		{"validate valid", []string{"validate", "-audit-log", audit, okScenario}, 0},
		{"validate not valid", []string{"validate", "-audit-log", audit, brokenScenario}, 1},
		{"validate host refused", []string{"validate", "-audit-log", audit, refusedScenario}, 1},
		{"validate no file", []string{"validate"}, 2},

		{"compare same", []string{"compare", baseline, baseline}, 0},
		{"compare regression", []string{"compare", baseline, worse}, 1},
		{"compare unreadable report", []string{"compare", baseline, filepath.Join(dir, "missing.json")}, 1},
		{"compare one report", []string{"compare", baseline}, 2},

		{"diagnose report", []string{"diagnose", "-no-llm", baseline}, 0},
		{"diagnose missing report", []string{"diagnose", "-no-llm", filepath.Join(dir, "missing.json")}, 1},
		{"diagnose bad flag", []string{"diagnose", "-no-such-flag"}, 2},
		{"new bad flag", []string{"new", "-no-such-flag"}, 2},
		{"protocols", []string{"protocols"}, 0},
		{"protocols unknown", []string{"protocols", "nope"}, 2},
		{"doctor bad output", []string{"doctor", "-output", "yaml"}, 2},
		{"import nothing", []string{"import"}, 2},
		{"init bad flag", []string{"init", "-no-such-flag"}, 2},
		{"live bad flag", []string{"live", "-no-such-flag"}, 2},
		{"mcp no subcommand", []string{"mcp"}, 2},
	}

	// No terminal answers the question about a refused -target.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	oldStdin := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = oldStdin }()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, stdout, stderr := captureCmd(t, func() int { return run(tc.args) })
			if got != tc.want {
				t.Fatalf("vegaload %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(tc.args, " "), got, tc.want, stdout, stderr)
			}
		})
	}
}

// A streamed run stopped by a signal exits as a process killed by it would.
func TestExitCodes_Interrupted(t *testing.T) {
	for sig, want := range map[os.Signal]int{os.Interrupt: 130, syscall.SIGTERM: 143} {
		st := &streaming{rec: stream.NewRecorder(stream.RecorderOptions{RunID: "sig-1"})}
		if got := st.interrupt(sig); got != want {
			t.Errorf("%v: exit %d, want %d", sig, got, want)
		}
	}
}

// With -output json, a refused host is a JSON object on stdout that lists
// it, for a program to ask a person about; the message on stderr is the one
// a person reads.
func TestRefusalIsJSONOnStdout(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	oldStdin := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = oldStdin }()

	dir := t.TempDir()
	scenario := filepath.Join(dir, "refused.vl.js")
	if err := os.WriteFile(scenario, []byte(`export default function () { http.get("http://Staging.Example.com/"); }`), 0o644); err != nil {
		t.Fatal(err)
	}
	base := []string{"run", "-no-report", "-no-live", "-audit-log", filepath.Join(dir, "audit.log"), "-executor", "constant-arrival-rate", "-rate", "20", "-max-vus", "2", "-duration", "30s"}
	for name, extra := range map[string][]string{
		"target":   {"-output", "json", "-target", "https://staging.example.com:8443/", "-protocol", "http1"},
		"scenario": {"-output", "json", scenario},
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			code, stdout, stderr := captureCmd(t, func() int { return run(append(append([]string(nil), base...), extra...)) })
			if code != 2 {
				t.Fatalf("exit %d, stderr %s", code, stderr)
			}
			if time.Since(start) > 10*time.Second {
				t.Fatalf("took %s: a refused host must stop the run", time.Since(start))
			}
			var got refusalJSON
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("stdout is not one JSON object: %q", stdout)
			}
			if len(got.RefusedHosts) != 1 || got.RefusedHosts[0] != "staging.example.com" || got.Error == "" {
				t.Fatalf("refusal %+v", got)
			}
			if !strings.Contains(stderr, "not localhost or allowlisted") && !strings.Contains(stderr, "without confirmation") {
				t.Fatalf("stderr %q", stderr)
			}
		})
	}

	// With the host approved, the same run goes ahead.
	code, _, stderr := captureCmd(t, func() int {
		return run(append(append([]string(nil), base[:len(base)-2]...), "-duration", "200ms", "-allow-target", "staging.example.com",
			"-target", "http://127.0.0.1:1/", "-protocol", "http1"))
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}
