package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/vegaload/vegaload/internal/safety"
	"github.com/vegaload/vegaload/internal/scripting/netapi"
)

// hostLog records the hosts a scenario's calls ask for, as the safety check
// every call runs (buildSafetyCheck) sees them: the ones it allowed and the
// ones it refused, each once, in first-seen order. validate reports them, and
// a run stops at the first refused one.
type hostLog struct {
	mu       sync.Mutex
	allowed  []string
	refused  []string
	seen     map[string]bool
	onRefuse func() // called outside the lock, after a host is refused
}

func newHostLog() *hostLog { return &hostLog{seen: map[string]bool{}} }

// wrap returns check, recording each host it is asked about.
func (h *hostLog) wrap(check netapi.SafetyCheck) netapi.SafetyCheck {
	return func(host string) error {
		err := check(host)
		h.record(host, err)
		return err
	}
}

func (h *hostLog) record(host string, err error) {
	host = strings.ToLower(host)
	refused := safety.IsHostRefusal(err)
	h.mu.Lock()
	key := "allowed:" + host
	if refused {
		key = "refused:" + host
	}
	if !h.seen[key] {
		h.seen[key] = true
		if refused {
			h.refused = append(h.refused, host)
		} else {
			h.allowed = append(h.allowed, host)
		}
	}
	onRefuse := h.onRefuse
	h.mu.Unlock()
	if refused && onRefuse != nil {
		onRefuse()
	}
}

func (h *hostLog) setOnRefuse(f func()) {
	h.mu.Lock()
	h.onRefuse = f
	h.mu.Unlock()
}

// Hosts returns copies of the allowed and refused hosts.
func (h *hostLog) Hosts() (allowed, refused []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.allowed...), append([]string(nil), h.refused...)
}

// refusalError is a run the safety rules stopped because of hosts that are
// neither localhost nor allowlisted. A person approves them with -allow-target
// (or -yes); the hosts are listed so that a program can ask for that approval
// without reading the message.
type refusalError struct {
	msg   string
	hosts []string
}

func (e *refusalError) Error() string { return e.msg }

// scenarioRefusal is the error of a scenario run stopped at refused hosts.
func scenarioRefusal(hosts []string) error {
	return &refusalError{msg: "stopped the run: " + refusalMessage(hosts), hosts: hosts}
}

// refusalMessage says which hosts were refused, in the words of
// safety.HostRefusal.
func refusalMessage(hosts []string) string {
	if len(hosts) == 1 {
		return safety.HostRefusal(hosts[0]).Error()
	}
	quoted := make([]string, len(hosts))
	for i, h := range hosts {
		quoted[i] = fmt.Sprintf("%q", h)
	}
	return fmt.Sprintf("hosts %s are not localhost or allowlisted (pass -yes, or add each with -allow-target)", strings.Join(quoted, ", "))
}

// refusedHosts returns the hosts err refused, or nil when it is not a refusal.
func refusedHosts(err error) []string {
	var re *refusalError
	if errors.As(err, &re) {
		return re.hosts
	}
	return nil
}

// refusalJSON is what `run -output json` prints on stdout, instead of a
// result, when the safety rules refused a host.
type refusalJSON struct {
	Error        string   `json:"error"`
	RefusedHosts []string `json:"refused_hosts"`
}

// writeRefusal prints the refusal for a machine reader: one JSON object for
// -output json, a run_failed line for -output jsonl, and nothing for text,
// whose message is on stderr.
func writeRefusal(w io.Writer, outputMode string, err error) {
	hosts := refusedHosts(err)
	if hosts == nil {
		return
	}
	body := refusalJSON{Error: err.Error(), RefusedHosts: hosts}
	switch outputMode {
	case "json":
		_ = json.NewEncoder(w).Encode(body)
	case "jsonl":
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "run_failed", "data": body})
	}
}
