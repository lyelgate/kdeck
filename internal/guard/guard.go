// Package guard protects production clusters. It classifies every command
// as a read, a write (changes the cluster) or an exec (runs commands inside
// pods), then applies the rules:
//
//   - Read-only (--ro, KP_RO=1, or a context matching safety.readOnly):
//     writes and execs are refused.
//   - Production (a context matching safety.production): writes need the
//     context name typed back; "apply" shows a diff first.
//   - Every write and exec is appended to ~/.kp/history.jsonl with its exit
//     code and, where possible, the command that undoes it.
package guard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lyelgate/kubepilot/internal/config"
	"github.com/lyelgate/kubepilot/internal/match"
)

// Kind of command.
type Kind int

const (
	Read Kind = iota
	Write
	Exec
)

func (k Kind) String() string { return [...]string{"read", "write", "exec"}[k] }

// writeVerbs change cluster state. A nil set means "any subcommand"; a
// non-nil set lists the subcommands that write.
var writeVerbs = map[string]map[string]bool{
	"apply": nil, "create": nil, "delete": nil, "edit": nil, "patch": nil, "replace": nil,
	"scale": nil, "autoscale": nil, "expose": nil, "run": nil, "label": nil, "annotate": nil,
	"taint": nil, "drain": nil, "cordon": nil, "uncordon": nil, "set": nil, "debug": nil, "cp": nil,
	"rollout":     {"restart": true, "undo": true, "pause": true, "resume": true},
	"certificate": {"approve": true, "deny": true},
	// kp commands that change things.
	"restart": nil, "clean": nil, "edit-secret": nil,
}

var execVerbs = map[string]bool{"exec": true, "attach": true, "sh": true, "shell": true, "kpsh": true, "each": true, "node-sh": true, "netshoot": true}

// Classify returns the kind of a fully expanded command line and the
// words that identify it ("rollout restart").
func Classify(args []string) (Kind, string) {
	words := positional(args)
	if len(words) == 0 {
		return Read, ""
	}
	verb := words[0]
	if hasDryRun(args) || verb == "diff" || verb == "help" || hasFlag(args, "--help") || hasFlag(args, "-h") {
		return Read, verb
	}
	if execVerbs[verb] {
		return Exec, verb
	}
	subs, ok := writeVerbs[verb]
	if !ok {
		return Read, verb
	}
	if verb == "apply" && len(words) > 1 && (words[1] == "view-last-applied") {
		return Read, "apply view-last-applied"
	}
	if verb == "clean" && !hasFlag(args, "--yes") {
		return Read, "clean (dry run)"
	}
	if subs == nil {
		return Write, verb
	}
	if len(words) > 1 && subs[words[1]] {
		return Write, verb + " " + words[1]
	}
	return Read, verb
}

// globalValueFlags take a separate value and must be skipped when looking
// for the verb.
var globalValueFlags = map[string]bool{
	"-n": true, "--namespace": true, "--context": true, "--kubeconfig": true, "--cluster": true,
	"--user": true, "-s": true, "--server": true, "--token": true, "--as": true, "--as-group": true,
	"--request-timeout": true, "-C": true, "--contexts": true,
}

func positional(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			if globalValueFlags[a] {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

func hasDryRun(args []string) bool {
	for i, a := range args {
		if a == "--" {
			return false
		}
		if strings.HasPrefix(a, "--dry-run") {
			v := strings.TrimPrefix(a, "--dry-run")
			v = strings.TrimPrefix(v, "=")
			if v == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				v = args[i+1]
			}
			return v != "none" && v != "false"
		}
	}
	return false
}

// Policy is the safety configuration resolved for one context.
type Policy struct {
	Context    string
	Production bool
	ReadOnly   bool
	Reason     string // why read-only
	DiffApply  bool
}

// Resolve applies the safety settings to a context name.
func Resolve(cfg config.SafetyConfig, ctx string, roFlag bool) Policy {
	p := Policy{Context: ctx, DiffApply: cfg.DiffBeforeApply == nil || *cfg.DiffBeforeApply}
	for _, pat := range cfg.Production {
		if m, err := match.Compile(pat); err == nil && m.Match(ctx) {
			p.Production = true
		}
	}
	switch {
	case roFlag:
		p.ReadOnly, p.Reason = true, "--ro"
	case os.Getenv("KP_RO") != "" && os.Getenv("KP_RO") != "0":
		p.ReadOnly, p.Reason = true, "KP_RO"
	default:
		for _, pat := range cfg.ReadOnly {
			if m, err := match.Compile(pat); err == nil && m.Match(ctx) {
				p.ReadOnly, p.Reason = true, "safety.readOnly matches "+pat
			}
		}
	}
	return p
}

// ErrRefused is returned when a command is not allowed to run.
type ErrRefused struct{ Msg string }

func (e ErrRefused) Error() string { return e.Msg }

// Check decides whether a command may run. confirm is called for
// production writes; it returns true when the user typed the context name.
func Check(p Policy, kind Kind, what string, confirm func(Policy, string) bool) error {
	if kind == Read {
		return nil
	}
	if p.ReadOnly {
		return ErrRefused{fmt.Sprintf("read-only (%s): refusing %s %q on context %q", p.Reason, kind, what, p.Context)}
	}
	if kind == Write && p.Production {
		if os.Getenv("KP_CONFIRM_CONTEXT") == p.Context {
			return nil
		}
		if !confirm(p, what) {
			return ErrRefused{fmt.Sprintf("not confirmed: %q on production context %q (in scripts set KP_CONFIRM_CONTEXT=%s)", what, p.Context, p.Context)}
		}
	}
	return nil
}

// Confirm asks on the terminal for the context name to be typed back.
func Confirm(in io.Reader, out io.Writer, interactive bool) func(Policy, string) bool {
	return func(p Policy, what string) bool {
		if !interactive {
			return false
		}
		fmt.Fprintf(out, "\x1b[41;97m PRODUCTION \x1b[0m %s on context \x1b[1m%s\x1b[0m\nType the context name to continue: ", what, p.Context)
		line, _ := bufio.NewReader(in).ReadString('\n')
		return strings.TrimSpace(line) == p.Context
	}
}

// Entry is one line of ~/.kp/history.jsonl.
type Entry struct {
	Time      time.Time `json:"time"`
	Context   string    `json:"context"`
	Namespace string    `json:"namespace,omitempty"`
	Kind      string    `json:"kind"`
	Command   []string  `json:"command"`
	Exit      int       `json:"exit"`
	Undo      string    `json:"undo,omitempty"`
}

// Record appends an entry; failures to write history never block commands.
func Record(e Entry) {
	f, err := os.OpenFile(config.Path("history.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(e)
	_, _ = f.Write(append(data, '\n'))
}

// History reads entries, newest last.
func History() ([]Entry, error) {
	f, err := os.Open(config.Path("history.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// UndoHint returns a command that reverses a write, when one is known
// without extra lookups. prevReplicas is filled by the caller for scale.
func UndoHint(args []string, ctx string, prevReplicas string) string {
	words := positional(args)
	if len(words) == 0 {
		return ""
	}
	target := strings.Join(words[1:], " ")
	ns := ""
	for i, a := range args {
		if (a == "-n" || a == "--namespace") && i+1 < len(args) {
			ns = " -n " + args[i+1]
		} else if strings.HasPrefix(a, "--namespace=") {
			ns = " -n " + strings.TrimPrefix(a, "--namespace=")
		}
	}
	switch {
	case words[0] == "scale" && prevReplicas != "":
		return fmt.Sprintf("kp --context %s scale %s --replicas=%s%s", ctx, target, prevReplicas, ns)
	case words[0] == "rollout" && len(words) > 2 && (words[1] == "restart" || words[1] == "undo"):
		return fmt.Sprintf("kp --context %s rollout undo %s%s", ctx, strings.Join(words[2:], " "), ns)
	case words[0] == "cordon" && len(words) > 1:
		return fmt.Sprintf("kp --context %s uncordon %s", ctx, strings.Join(words[1:], " "))
	case words[0] == "uncordon" && len(words) > 1:
		return fmt.Sprintf("kp --context %s cordon %s", ctx, strings.Join(words[1:], " "))
	case words[0] == "drain" && len(words) > 1:
		return fmt.Sprintf("kp --context %s uncordon %s", ctx, strings.Join(words[1:], " "))
	}
	return ""
}

// ScaleTarget returns the TYPE/NAME a scale command acts on, for reading the
// replica count before it changes.
func ScaleTarget(args []string) string {
	words := positional(args)
	if len(words) < 2 || words[0] != "scale" {
		return ""
	}
	if strings.Contains(words[1], "/") {
		return words[1]
	}
	if len(words) >= 3 {
		return words[1] + "/" + words[2]
	}
	return ""
}

// ApplyDiffArgs turns "apply <args>" into "diff <args>" for a preview.
func ApplyDiffArgs(args []string) []string {
	out := make([]string, 0, len(args))
	swapped := false
	for _, a := range args {
		if !swapped && a == "apply" {
			out = append(out, "diff")
			swapped = true
			continue
		}
		if a == "--server-side" || strings.HasPrefix(a, "--force-conflicts") || strings.HasPrefix(a, "--prune") || strings.HasPrefix(a, "--wait") {
			continue // flags diff does not accept
		}
		out = append(out, a)
	}
	return out
}
