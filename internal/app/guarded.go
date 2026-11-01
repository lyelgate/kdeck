package app

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/lyelgate/kdeck/internal/config"
	"github.com/lyelgate/kdeck/internal/guard"
	"github.com/lyelgate/kdeck/internal/match"
)

// kpFlags are kdeck-level flags removed before kubectl sees the arguments.
type kpFlags struct {
	readOnly bool
	contexts string // -C / --contexts regex for multi-cluster runs
}

func extractKPFlags(args []string) (kpFlags, []string) {
	var f kpFlags
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		switch {
		case a == "--ro" || a == "--read-only":
			f.readOnly = true
		case (a == "-C" || a == "--contexts") && i+1 < len(args):
			f.contexts = args[i+1]
			i++
		case strings.HasPrefix(a, "--contexts="):
			f.contexts = strings.TrimPrefix(a, "--contexts=")
		default:
			out = append(out, a)
		}
	}
	return f, out
}

func flagValue(args []string, names ...string) string {
	for i, a := range args {
		if a == "--" {
			return ""
		}
		for _, n := range names {
			if a == n && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(a, n+"=") {
				return strings.TrimPrefix(a, n+"=")
			}
		}
	}
	return ""
}

func loadKubeconfig(args []string) (clientcmd.ClientConfig, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if p := flagValue(args, "--kubeconfig"); p != "" {
		rules.ExplicitPath = p
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}), nil
}

// targetContext is the context a command will use.
func targetContext(args []string) string {
	if c := flagValue(args, "--context"); c != "" {
		return c
	}
	cc, _ := loadKubeconfig(args)
	raw, err := cc.RawConfig()
	if err != nil {
		return ""
	}
	return raw.CurrentContext
}

func stdinIsTTY() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// guardCommand enforces read-only and production rules before a command
// runs. It returns the history entry to complete after the run (nil when
// the command is a read).
func guardCommand(cfg *config.Config, f kpFlags, args []string) (*guard.Entry, error) {
	kind, what := guard.Classify(args)
	if kind == guard.Read || os.Getenv("KDECK_INTERNAL_NOGUARD") == "1" {
		return nil, nil
	}
	ctx := targetContext(args)
	p := guard.Resolve(cfg.Safety, ctx, f.readOnly)

	// Preview apply on production before asking.
	if kind == guard.Write && p.Production && p.DiffApply && !p.ReadOnly && strings.HasPrefix(what, "apply") && os.Getenv("KDECK_CONFIRM_CONTEXT") != ctx {
		fmt.Fprintf(os.Stderr, "Diff against production context %q:\n", ctx)
		if self, err := os.Executable(); err == nil {
			c := exec.Command(self, guard.ApplyDiffArgs(args)...)
			c.Env = append(os.Environ(), "KDECK_INTERNAL_NOGUARD=1")
			c.Stdout, c.Stderr = os.Stderr, os.Stderr
			err := c.Run()
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() > 1 {
				return nil, fmt.Errorf("kubectl diff failed; not applying")
			} else if err == nil {
				fmt.Fprintln(os.Stderr, "(no changes)")
			}
		}
	}
	if err := guard.Check(p, kind, what, guard.Confirm(os.Stdin, os.Stderr, stdinIsTTY())); err != nil {
		return nil, err
	}
	e := &guard.Entry{Time: time.Now().UTC(), Context: ctx, Namespace: flagValue(args, "-n", "--namespace"), Kind: kind.String(), Command: args}
	prev := ""
	if t := guard.ScaleTarget(args); t != "" {
		var buf bytes.Buffer
		kargs := append(globalFlags(args), "get", t, "-o", "jsonpath={.spec.replicas}")
		if err := runKubectl(&buf, kargs...); err == nil {
			prev = strings.TrimSpace(buf.String())
		}
	}
	e.Undo = guard.UndoHint(args, ctx, prev)
	return e, nil
}

// --- multi-cluster ---------------------------------------------------------------

var headerRe = regexp.MustCompile(`^[A-Z][A-Z0-9 _()/.-]*$`)

type ctxResult struct {
	ctx            string
	stdout, stderr []byte
	exit           int
}

// runMulti runs the command once per context matching pattern, in parallel,
// and merges the output with a CONTEXT column.
func runMulti(cfg *config.Config, f kpFlags, args []string) int {
	cc, _ := loadKubeconfig(args)
	raw, err := cc.RawConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	m, err := match.Compile(f.contexts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	var ctxs []string
	for n := range raw.Contexts {
		if m.Match(n) {
			ctxs = append(ctxs, n)
		}
	}
	sort.Strings(ctxs)
	if len(ctxs) == 0 {
		fmt.Fprintf(os.Stderr, "error: no context matches %q\n", f.contexts)
		return 1
	}

	kind, what := guard.Classify(args)
	if kind == guard.Exec && hasTTYFlag(args) {
		fmt.Fprintln(os.Stderr, "error: interactive exec cannot fan out to several contexts; drop -t/-it")
		return 1
	}
	if kind != guard.Read {
		var prod []string
		for _, c := range ctxs {
			p := guard.Resolve(cfg.Safety, c, f.readOnly)
			if p.ReadOnly {
				fmt.Fprintf(os.Stderr, "error: context %q is read-only (%s); refusing %s across contexts\n", c, p.Reason, what)
				return 1
			}
			if p.Production {
				prod = append(prod, c)
			}
		}
		if kind == guard.Write {
			fmt.Fprintf(os.Stderr, "%s will run on %d contexts: %s\n", what, len(ctxs), strings.Join(ctxs, ", "))
			if len(prod) > 0 || len(ctxs) > 1 {
				if !stdinIsTTY() && os.Getenv("KDECK_CONFIRM_MULTI") != fmt.Sprint(len(ctxs)) {
					fmt.Fprintf(os.Stderr, "error: not confirmed (in scripts set KDECK_CONFIRM_MULTI=%d)\n", len(ctxs))
					return 1
				}
				if stdinIsTTY() {
					fmt.Fprintf(os.Stderr, "Type %d to run on all of them: ", len(ctxs))
					line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
					if strings.TrimSpace(line) != fmt.Sprint(len(ctxs)) {
						fmt.Fprintln(os.Stderr, "error: not confirmed")
						return 1
					}
				}
			}
		}
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	results := make([]ctxResult, len(ctxs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, c := range ctxs {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cmd := exec.Command(self, append([]string{"--context", c}, args...)...)
			cmd.Env = append(os.Environ(), "KDECK_CONFIRM_CONTEXT="+c, "KDECK_MULTI_CHILD=1", "KDECK_NO_FZF=1")
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			err := cmd.Run()
			r := ctxResult{ctx: c, stdout: out.Bytes(), stderr: errb.Bytes()}
			if ee, ok := err.(*exec.ExitError); ok {
				r.exit = ee.ExitCode()
			} else if err != nil {
				r.exit, r.stderr = 1, []byte(err.Error())
			}
			results[i] = r
		}(i, c)
	}
	wg.Wait()
	return mergeOutput(results)
}

func hasTTYFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-t" || a == "-it" || a == "-ti" || a == "--tty" {
			return true
		}
	}
	return false
}

func mergeOutput(results []ctxResult) int {
	width := len("CONTEXT")
	for _, r := range results {
		if len(r.ctx) > width {
			width = len(r.ctx)
		}
	}
	pad := func(s string) string { return s + strings.Repeat(" ", width-len(s)+2) }
	headerPrinted := false
	exit := 0
	for _, r := range results {
		lines := strings.Split(strings.TrimRight(string(r.stdout), "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
		for i, l := range lines {
			if i == 0 && headerRe.MatchString(strings.TrimSpace(firstField(l))) && strings.ToUpper(l) == l {
				if !headerPrinted {
					fmt.Println(pad("CONTEXT") + l)
					headerPrinted = true
				}
				continue
			}
			fmt.Println(pad(r.ctx) + l)
		}
		for _, l := range strings.Split(strings.TrimRight(string(r.stderr), "\n"), "\n") {
			if l != "" {
				fmt.Fprintf(os.Stderr, "%s: %s\n", r.ctx, l)
			}
		}
		if r.exit > exit {
			exit = r.exit
		}
	}
	return exit
}

func firstField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
