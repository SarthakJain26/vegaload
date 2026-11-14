package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// approvalArgs are the arguments older versions of run_test and
// validate_scenario took to approve a host. Only a person may approve a host,
// so the tools refuse them instead of quietly dropping them.
var approvalArgs = []string{"yes", "allow_targets"}

// refuseApprovalArgs returns an error when raw names an approval argument.
func refuseApprovalArgs(raw json.RawMessage) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil // the tool's own parsing reports it
	}
	for _, k := range approvalArgs {
		if _, ok := keys[k]; ok {
			return fmt.Errorf("%q is not an argument of this tool: an agent cannot approve a host by itself. "+
				"Call the tool again without it. When a host is not localhost, VegaLoad asks the user to approve it, "+
				"or, if this MCP client cannot ask, says which command the user can run in their own terminal", k)
		}
	}
	return nil
}

// maxApprovalRounds bounds how many times one tool call asks the person. A
// scenario can name a new host after the last one was approved, so it may
// take more than one round.
const maxApprovalRounds = 5

// runApproved runs `vegaload <flags...> <positional...>`. When the CLI
// refuses hosts (its JSON output lists refused_hosts), it asks the person
// using the MCP client to approve them (Confirm) and, only when they do,
// runs the same command again with one -allow-target per approved host and
// nothing else added. When the client cannot ask, it returns an error that
// gives the agent the command the person can run in a terminal instead.
//
// This is the same CLI command a human types; the tool only cannot add the
// approval on the agent's own say-so (AGENTS.md, rule 3).
func runApproved(ctx context.Context, exePath string, flags, positional []string) (stdout, stderr []byte, err error) {
	var approved []string
	for round := 0; ; round++ {
		args := commandArgs(flags, approved, positional)
		stdout, stderr, err = RunCLISplit(ctx, exePath, args...)
		hosts := refusedHostsIn(stdout)
		if len(hosts) == 0 {
			return stdout, stderr, err
		}
		for _, h := range hosts {
			if containsString(approved, h) {
				return nil, nil, fmt.Errorf("vegaload still refused %s after the user approved it", h)
			}
		}
		if round >= maxApprovalRounds {
			return nil, nil, fmt.Errorf("vegaload refused %s after %d approvals; stopping", strings.Join(hosts, ", "), round)
		}
		ok, cerr := Confirm(ctx, approvalQuestion(hosts, humanCommand(flags, approved, positional)))
		if errors.Is(cerr, ErrNoElicitation) {
			return nil, nil, needsPersonError(hosts, humanCommand(flags, append(append([]string(nil), approved...), hosts...), positional))
		}
		if cerr != nil {
			return nil, nil, fmt.Errorf("asking the user to approve %s: %w", strings.Join(hosts, ", "), cerr)
		}
		if !ok {
			return nil, nil, fmt.Errorf("the user did not approve %s, so VegaLoad sent nothing to it. "+
				"Do not try again unless the user asks for it", strings.Join(hosts, ", "))
		}
		approved = append(approved, hosts...)
	}
}

func commandArgs(flags, allow, positional []string) []string {
	args := append([]string(nil), flags...)
	for _, h := range allow {
		args = append(args, "-allow-target", h)
	}
	return append(args, positional...) // positional last: flag parsing stops at the first non-flag
}

// refusedHostsIn reads the refused_hosts list from a command's JSON output:
// the refusal object of `run -output json`, or the result of
// `validate -output json`.
func refusedHostsIn(stdout []byte) []string {
	var out struct {
		RefusedHosts []string `json:"refused_hosts"`
	}
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil
	}
	return out.RefusedHosts
}

func approvalQuestion(hosts []string, command string) string {
	what := "host " + hosts[0] + ", which is"
	if len(hosts) > 1 {
		what = "hosts " + strings.Join(hosts, ", ") + ", which are"
	}
	return fmt.Sprintf("Your agent asked VegaLoad to send load to %s not localhost and not on the allow list. "+
		"Allow it for this one run?\n\nThe command: %s", what, command)
}

func needsPersonError(hosts []string, command string) error {
	return fmt.Errorf("VegaLoad refused %s: a host that is not localhost needs a person's approval, "+
		"and this MCP client cannot ask the user (it does not support elicitation). "+
		"Ask the user to run this command in their own terminal, where -allow-target is their approval:\n\n  %s\n\n"+
		"Do not call the tool again with yes or allow_targets: it does not take them",
		strings.Join(hosts, ", "), command)
}

// humanCommand is the command for a person to type: the tool's own flags
// for parsing its output and for the audit log are left out, so the audit
// log shows that a person ran it.
func humanCommand(flags, allow, positional []string) string {
	var kept []string
	for i := 0; i < len(flags); i++ {
		if i+1 < len(flags) && ((flags[i] == "-output" && flags[i+1] == "json") || (flags[i] == "-trigger" && flags[i+1] == "mcp")) {
			i++
			continue
		}
		kept = append(kept, flags[i])
	}
	parts := []string{"vegaload"}
	for _, a := range commandArgs(kept, allow, positional) {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@,+%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
