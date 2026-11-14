package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"

	"github.com/vegaload/vegaload/internal/stream"
)

// versionInfo is what `vegaload version -output json` prints. A program that
// drives VegaLoad reads it to learn what this binary can do before it calls
// anything else.
type versionInfo struct {
	Version  string            `json:"version"`
	Schemas  map[string]string `json:"schemas"`
	Features []string          `json:"features"`
	Go       string            `json:"go"`
	OS       string            `json:"os"`
	Arch     string            `json:"arch"`
}

// features lists the capabilities of this binary as stable names, sorted. A
// name is never renamed or reused: a caller tests for it with a plain string
// match. Add a name here when a capability ships.
func features() []string {
	out := []string{
		"abort-on-breach",
		"baseline",
		"checks",
		"data-files",
		"exit-codes-v1",
		"junit",
		"live-view",
		"mcp-elicitation",
		"run-started-definition",
		"steps",
		"stream-v1",
		"thresholds",
		"validate-json",
		"version-json",
	}
	for _, d := range drivers {
		out = append(out, "protocols:"+d.name)
	}
	sort.Strings(out)
	return out
}

func newVersionInfo() versionInfo {
	return versionInfo{
		Version:  version,
		Schemas:  map[string]string{"stream": stream.Schema},
		Features: features(),
		Go:       runtime.Version(),
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
	}
}

func cmdVersion(args []string) int {
	return runVersion(args, os.Stdout, os.Stderr)
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	output := fs.String("output", "text", "output mode: text or json (version, schemas, features, Go version, OS and architecture)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: vegaload version [-output text|json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "vegaload version: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	switch *output {
	case "text":
		fmt.Fprintf(stdout, "vegaload %s\n", version)
	case "json":
		if err := json.NewEncoder(stdout).Encode(newVersionInfo()); err != nil {
			fmt.Fprintf(stderr, "vegaload version: %v\n", err)
			return 1
		}
	default:
		fmt.Fprintf(stderr, "vegaload version: -output %q: want text or json\n", *output)
		return 2
	}
	return 0
}
