// inputs.go wires FR-CLI-17's flags (-data, -env, -secret-env) into a run.
// `run` and `validate` share them, so a scenario that validates with a set
// of flags runs with the same ones.
package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/vegaload/vegaload/internal/inputs"
)

// scriptInputFlags holds the raw values of the three flags until the
// files are read and the variables are looked up.
type scriptInputFlags struct {
	data      repeatedFlags
	env       repeatedFlags
	secretEnv repeatedFlags
	pick      string
}

func (f *scriptInputFlags) register(fs *flag.FlagSet) {
	fs.Var(&f.data, "data", "a CSV (header row) or JSON (array of objects) file. A scenario reads rows as data.NAME.next() or data.NAME.random(). With -target and -protocol, a call reads them as {data.NAME.COLUMN}. NAME is the file name without its extension, or give one as name=path (repeatable)")
	fs.Var(&f.env, "env", "an environment variable scripts may read as env.NAME; a comma-separated list is fine (repeatable). Only variables named here, or with -secret-env, are visible to scripts")
	fs.Var(&f.secretEnv, "secret-env", "like -env, but the value is a secret: it is removed from the summary, JSON, audit log and console output (repeatable)")
	fs.StringVar(&f.pick, "data-pick", "", "with -target and -protocol: how each call picks a row, next (the default, shared by every user) or random")
}

func (f *scriptInputFlags) used() bool {
	return len(f.data) > 0 || len(f.env) > 0 || len(f.secretEnv) > 0
}

// inputNames is all a run may say about its -data, -env and -secret-env: the
// data files' names and row counts and the variable names, never a value.
type inputNames struct {
	Data      []inputFile `json:"data,omitempty"`
	Env       []string    `json:"env,omitempty"`
	SecretEnv []string    `json:"secret_env,omitempty"`
}

// inputFile is one data file: the NAME a scenario reads as data.NAME, the
// file's base name, and its row count.
type inputFile struct {
	Name string `json:"name"`
	File string `json:"file"`
	Rows int    `json:"rows"`
}

func dataFileNames(sets map[string]*inputs.Set) []inputFile {
	var out []inputFile
	for _, name := range sortedNames(sets) {
		s := sets[name]
		out = append(out, inputFile{Name: name, File: s.FileName(), Rows: s.Len()})
	}
	return out
}

// envNames splits the -env and -secret-env lists into sorted names. A name in
// both is a secret, as inputs.ResolveEnv treats it.
func envNames(plain, secret []string) (env, secretEnv []string) {
	split := func(list []string) map[string]bool {
		out := map[string]bool{}
		for _, item := range list {
			for _, name := range strings.Split(item, ",") {
				if name = strings.TrimSpace(name); name != "" {
					out[name] = true
				}
			}
		}
		return out
	}
	p, s := split(plain), split(secret)
	for name := range p {
		if s[name] {
			delete(p, name)
		}
	}
	return sortedNames(p), sortedNames(s)
}

func sortedNames[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolve reads the data files and looks up the variables, and stores the
// result on cfg. It must run before any load is sent, so a missing file or
// unset variable is a usage error.
func (f *scriptInputFlags) resolve(cfg *runConfig) error {
	if f.pick != "" && f.pick != "next" && f.pick != "random" {
		return fmt.Errorf("-data-pick %q: want next or random", f.pick)
	}
	if cfg.ScenarioPath != "" {
		if f.pick != "" {
			return fmt.Errorf("-data-pick is for -target and -protocol; a scenario reads data.NAME itself")
		}
		if !f.used() {
			return nil
		}
		sets, err := inputs.ParseData([]string(f.data))
		if err != nil {
			return err
		}
		env, err := inputs.ResolveEnv([]string(f.env), []string(f.secretEnv))
		if err != nil {
			return err
		}
		if in := inputs.New(env, sets); in != nil {
			cfg.Inputs = in // not assigned when nil: a nil *Inputs inside the interface would not compare equal to nil
		}
		cfg.inputNames.Data = dataFileNames(sets)
		cfg.inputNames.Env, cfg.inputNames.SecretEnv = envNames([]string(f.env), []string(f.secretEnv))
		return nil
	}
	if len(f.env) > 0 || len(f.secretEnv) > 0 {
		return fmt.Errorf("-env and -secret-env are for scenario files. With -target and -protocol, only -data and -data-pick work")
	}
	if f.pick != "" && len(f.data) == 0 {
		return fmt.Errorf("-data-pick needs -data")
	}
	if len(f.data) == 0 {
		return nil
	}
	sets, err := inputs.ParseData([]string(f.data))
	if err != nil {
		return err
	}
	cfg.DataSets = sets
	cfg.inputNames.Data = dataFileNames(sets)
	cfg.DataPick = f.pick
	if cfg.DataPick == "" {
		cfg.DataPick = "next"
	}
	return nil
}
