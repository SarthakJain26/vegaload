package main

import (
	"bytes"
	"encoding/json"
	"github.com/vegaload/vegaload/internal/audit"
	"github.com/vegaload/vegaload/internal/report"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeScenario(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":7}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func validate(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	args = append([]string{"-audit-log", filepath.Join(t.TempDir(), "audit.log")}, args...)
	code := runValidate(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestValidate_ValidScenarioExitsZero(t *testing.T) {
	srv := okServer(t)
	p := writeScenario(t, "ok.vl.js", `export default function () {
  const r = http.get("`+srv.URL+`/");
  check(r, {"status is 200": (r) => r.status === 200});
}`)
	code, out, _ := validate(t, p)
	if code != 0 || !strings.Contains(out, "valid:") || !strings.Contains(out, "PASS  status is 200") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestValidate_JSONOutput(t *testing.T) {
	srv := okServer(t)
	p := writeScenario(t, "ok.vl.js", `export default function () { http.get("`+srv.URL+`/"); }`)
	code, out, _ := validate(t, "-output", "json", p)
	var res validateResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not json: %q", out)
	}
	if code != 0 || !res.Valid || res.Scenario != p || res.Elapsed == "" {
		t.Fatalf("code=%d res=%+v", code, res)
	}
}

func TestValidate_SyntaxErrorIsLoadStage(t *testing.T) {
	p := writeScenario(t, "bad.vl.js", `export default function( {`)
	code, out, _ := validate(t, "-output", "json", p)
	var res validateResult
	_ = json.Unmarshal([]byte(out), &res)
	if code != 1 || res.Valid || res.Stage != "load" || res.Error == "" {
		t.Fatalf("code=%d res=%+v", code, res)
	}
}

func TestValidate_MissingFileIsLoadStage(t *testing.T) {
	code, out, _ := validate(t, filepath.Join(t.TempDir(), "nope.vl.js"))
	if code != 1 || !strings.Contains(out, "not valid") || !strings.Contains(out, "(load)") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestValidate_ThrowingIterationIsIterationStage(t *testing.T) {
	p := writeScenario(t, "throw.vl.js", `export default function () { throw new Error("boom"); }`)
	code, out, _ := validate(t, "-output", "json", p)
	var res validateResult
	_ = json.Unmarshal([]byte(out), &res)
	if code != 1 || res.Stage != "iteration" || !strings.Contains(res.Error, "boom") {
		t.Fatalf("code=%d res=%+v", code, res)
	}
}

func TestValidate_RefusesHostOutsideAllowlist(t *testing.T) {
	p := writeScenario(t, "far.vl.js", `export default function () { http.get("http://example.com/"); }`)
	code, out, _ := validate(t, p)
	if code != 1 || !strings.Contains(out, "not localhost or allowlisted") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestValidate_FailedCheckDoesNotMakeItInvalid(t *testing.T) {
	srv := okServer(t)
	p := writeScenario(t, "chk.vl.js", `export default function () {
  check(http.get("`+srv.URL+`/"), {"status is 500": (r) => r.status === 500});
}`)
	code, out, _ := validate(t, p)
	if code != 0 || !strings.Contains(out, "FAIL  status is 500") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestValidate_UsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{{}, {"a.js", "b.js"}, {"-output", "xml", "a.js"}} {
		if code, _, _ := validate(t, args...); code != 2 {
			t.Errorf("args %v: code %d, want 2", args, code)
		}
	}
}

func TestValidate_PythonScenario(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 not available")
	}
	srv := okServer(t)
	p := writeScenario(t, "ok.py", `def iteration():
    r = http.get("`+srv.URL+`/")
    check(r, {"status is 200": lambda r: r.status == 200})
`)
	code, out, _ := validate(t, p)
	if code != 0 || !strings.Contains(out, "PASS  status is 200") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// readAudit returns the audit entries written to the log at path.
func readAudit(t *testing.T, path string) []audit.Entry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading audit log: %v", err)
	}
	var out []audit.Entry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e audit.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func validateAudited(t *testing.T, scenario string) audit.Entry {
	t.Helper()
	log := filepath.Join(t.TempDir(), "audit.log")
	var out, errb bytes.Buffer
	runValidate([]string{"-audit-log", log, scenario}, &out, &errb)
	entries := readAudit(t, log)
	if len(entries) != 1 {
		t.Fatalf("want 1 audit entry, got %d", len(entries))
	}
	return entries[0]
}

func TestValidate_AuditEntry_Valid(t *testing.T) {
	srv := okServer(t)
	p := writeScenario(t, "ok.vl.js", `export default function () { http.get("`+srv.URL+`/"); }`)
	e := validateAudited(t, p)
	if e.Outcome != "success" || e.Executor != "validate" || e.VUs != 1 || e.Total != 1 || e.Failed != 0 || e.Duration < 0 || e.ScenarioPath != p {
		t.Fatalf("entry = %+v", e)
	}
}

func TestValidate_AuditEntry_LoadFailure(t *testing.T) {
	p := writeScenario(t, "bad.vl.js", `export default function( {`)
	e := validateAudited(t, p)
	if e.Outcome != "error" || e.Total != 0 || e.Executor != "validate" || !strings.HasPrefix(e.Error, "load:") {
		t.Fatalf("entry = %+v", e)
	}
}

func TestValidate_AuditEntry_RefusedHostIsAnIterationError(t *testing.T) {
	p := writeScenario(t, "far.vl.js", `export default function () { http.get("http://example.com/"); }`)
	e := validateAudited(t, p)
	if e.Outcome != "error" || e.Total != 0 || !strings.HasPrefix(e.Error, "iteration:") || !strings.Contains(e.Error, "not localhost or allowlisted") {
		t.Fatalf("entry = %+v", e)
	}
}

func TestRecordAudit_WarningNamesTheCommand(t *testing.T) {
	// An audit path that cannot be written: a directory.
	cfg := &runConfig{AuditLogPath: t.TempDir(), Trigger: "cli"}
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	recordAudit("validate", cfg, &report.Result{}, nil)
	w.Close()
	os.Stderr = old
	buf := new(bytes.Buffer)
	buf.ReadFrom(r)
	if !strings.HasPrefix(buf.String(), "vegaload validate: warning") {
		t.Fatalf("stderr = %q", buf.String())
	}
}

func validateJSON(t *testing.T, args ...string) (int, validateResult) {
	t.Helper()
	code, out, errOut := validate(t, append([]string{"-output", "json"}, args...)...)
	var res validateResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not json: %q (stderr %q)", out, errOut)
	}
	return code, res
}

// The JSON names the resolved settings, the hosts reached, and of the inputs
// only names and row counts.
func TestValidate_JSONHasConfigAndHosts(t *testing.T) {
	srv := okServer(t)
	t.Setenv("VL_REGION", "eu-west-REGIONVALUE")
	t.Setenv("VL_TOKEN", "TOKENVALUE")
	data := writeScenario(t, "users.csv", "name\nann\nbob\ncy\n")
	p := writeScenario(t, "ok.vl.js", `export default function () {
  http.get("`+srv.URL+`/a");
  http.get("http://localhost:`+strings.Split(srv.URL, ":")[2]+`/b");
  http.get("`+srv.URL+`/c");
}`)
	code, res := validateJSON(t, "-timeout", "7s", "-env", "VL_REGION", "-secret-env", "VL_TOKEN", "-data", data, "-allow-target", "api.example.com", p)
	if code != 0 || !res.Valid {
		t.Fatalf("code=%d res=%+v", code, res)
	}
	c := res.Config
	if c.Scenario != p || c.Language != "javascript" || c.Executor != "validate" || c.VUs != 1 || c.Iterations != 1 || c.Timeout != "7s" {
		t.Fatalf("config %+v", c)
	}
	if strings.Join(c.Env, ",") != "VL_REGION" || strings.Join(c.SecretEnv, ",") != "VL_TOKEN" || strings.Join(c.AllowTargets, ",") != "api.example.com" {
		t.Fatalf("config %+v", c)
	}
	if len(c.DataFiles) != 1 || c.DataFiles[0] != (inputFile{Name: "users", File: "users.csv", Rows: 3}) {
		t.Fatalf("data files %+v", c.DataFiles)
	}
	if strings.Join(res.Hosts, ",") != "127.0.0.1,localhost" || len(res.RefusedHosts) != 0 {
		t.Fatalf("hosts %v refused %v", res.Hosts, res.RefusedHosts)
	}
	_, raw, _ := validate(t, "-output", "json", "-env", "VL_REGION", "-secret-env", "VL_TOKEN", "-data", data, p)
	for _, v := range []string{"REGIONVALUE", "TOKENVALUE", "ann", "bob"} {
		if strings.Contains(raw, v) {
			t.Fatalf("%q is in the output: %s", v, raw)
		}
	}
}

// A refused host is listed apart, and makes the scenario not valid even when
// the script catches the refusal: a real run would stop at it.
func TestValidate_RefusedHostsAreListed(t *testing.T) {
	srv := okServer(t)
	p := writeScenario(t, "far.vl.js", `export default function () {
  http.get("`+srv.URL+`/");
  try { http.get("http://Example.com/"); } catch (e) {}
  try { http.get("http://example.com/again"); } catch (e) {}
}`)
	code, res := validateJSON(t, p)
	if code != 1 || res.Valid || res.Stage != "iteration" || !strings.Contains(res.Error, `host "example.com" is not localhost or allowlisted`) {
		t.Fatalf("code=%d res=%+v", code, res)
	}
	if strings.Join(res.Hosts, ",") != "127.0.0.1" || strings.Join(res.RefusedHosts, ",") != "example.com" {
		t.Fatalf("hosts %v refused %v", res.Hosts, res.RefusedHosts)
	}
	if res.ErrorLocation != nil {
		t.Fatalf("a caught refusal has no line: %+v", res.ErrorLocation)
	}
	code, res = validateJSON(t, "-allow-target", "example.com", "-timeout", "1ms", p)
	if len(res.RefusedHosts) != 0 {
		t.Fatalf("allowed host still refused: code=%d %+v", code, res)
	}
}

// error_location points at the line and column of the scenario file the
// error came from: a syntax error, a throw, a refused call, in JavaScript and
// TypeScript, and is left out when the error is not from the file.
func TestValidate_ErrorLocation(t *testing.T) {
	cases := []struct {
		name, file, src string
		line, column    int
	}{
		{"syntax", "bad.vl.js", "export default function () {\n  const x = ;\n}\n", 2, 13},
		{"throw", "throw.vl.js", "// one\nexport default function () {\n  const a = 1;\n  throw new Error(\"boom\");\n}\n", 4, 9},
		{"refused call", "far.vl.js", "export default function () {\n  http.get(\"http://example.com/\");\n}\n", 2, 8},
		{"typescript", "throw.ts", "interface A { n: number }\nconst a: A = { n: 1 };\nexport default function (): void {\n  if (a.n === 1) {\n    throw new Error(\"ts\");\n  }\n}\n", 5, 11},
		{"top level", "top.vl.js", "const a = 1;\n\nthrow new Error(\"top\");\nexport default function () {}\n", 3, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeScenario(t, tc.file, tc.src)
			_, res := validateJSON(t, p)
			want := errorLocation{File: p, Line: tc.line, Column: tc.column}
			if res.Valid || res.ErrorLocation == nil || *res.ErrorLocation != want {
				t.Fatalf("location %+v, want %+v (error %q)", res.ErrorLocation, want, res.Error)
			}
			_, text, _ := validate(t, p)
			if !strings.Contains(text, "  at "+want.String()+"\n") {
				t.Fatalf("text output %q has no location %s", text, want)
			}
		})
	}

	p := writeScenario(t, "noexport.vl.js", "const a = 1;\n")
	if _, res := validateJSON(t, p); res.Valid || res.ErrorLocation != nil {
		t.Fatalf("an error not from a line has no location: %+v", res)
	}
}

func TestValidate_ErrorLocationPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cases := []struct {
		name, src  string
		line       int
		wantColumn bool
	}{
		{"raise", "import json\n\ndef iteration():\n    x = 1\n    raise ValueError(\"boom\")\n", 5, false},
		{"syntax", "def iteration(:\n    pass\n", 1, true},
		{"refused call", "def helper():\n    http.get(\"http://example.com/\")\n\ndef iteration():\n    helper()\n", 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeScenario(t, "s.py", tc.src)
			_, res := validateJSON(t, p)
			loc := res.ErrorLocation
			if res.Valid || loc == nil || loc.File != p || loc.Line != tc.line || (tc.wantColumn && loc.Column == 0) {
				t.Fatalf("location %+v, want line %d (error %q)", loc, tc.line, res.Error)
			}
		})
	}
}
