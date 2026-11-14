package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/vegaload/vegaload/internal/stream"
)

func TestVersion_Text(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runVersion(nil, &out, &errb); code != 0 || out.String() != "vegaload "+version+"\n" {
		t.Fatalf("code %d out %q", code, out.String())
	}
}

func TestVersion_JSON(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runVersion([]string{"-output", "json"}, &out, &errb); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not json: %q", out.String())
	}
	if got["version"] != version || got["go"] != runtime.Version() || got["os"] != runtime.GOOS || got["arch"] != runtime.GOARCH {
		t.Fatalf("%v", got)
	}
	if got["schemas"].(map[string]any)["stream"] != stream.Schema {
		t.Fatalf("schemas %v", got["schemas"])
	}
	var features []string
	for _, f := range got["features"].([]any) {
		features = append(features, f.(string))
	}
	if !sort.StringsAreSorted(features) {
		t.Fatalf("features not sorted: %v", features)
	}
	for _, want := range []string{"thresholds", "abort-on-breach", "baseline", "junit", "checks", "steps", "data-files", "stream-v1", "live-view", "validate-json", "version-json", "run-started-definition", "mcp-elicitation", "exit-codes-v1"} {
		if !contains(features, want) {
			t.Errorf("features lack %q: %v", want, features)
		}
	}
	for _, d := range drivers {
		if !contains(features, "protocols:"+d.name) {
			t.Errorf("features lack protocols:%s", d.name)
		}
	}
	seen := map[string]bool{}
	for _, f := range features {
		if seen[f] || strings.TrimSpace(f) != f || f == "" {
			t.Errorf("feature %q repeated or blank", f)
		}
		seen[f] = true
	}
}

func TestVersion_DoubleDashAndUsage(t *testing.T) {
	code, out, _ := captureCmd(t, func() int { return run([]string{"--version", "-output", "json"}) })
	if code != 0 || !strings.Contains(out, `"features"`) {
		t.Fatalf("code %d out %q", code, out)
	}
	for _, args := range [][]string{{"-output", "yaml"}, {"extra"}} {
		var o, e bytes.Buffer
		if code := runVersion(args, &o, &e); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
