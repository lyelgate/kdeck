package shell

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lyelgate/kdeck/internal/alias"
	"github.com/lyelgate/kdeck/internal/config"
	"github.com/lyelgate/kdeck/internal/match"
	"github.com/lyelgate/kdeck/internal/quick"
)

// row is one line of the reference: a shortcut, the command it stands for,
// and a note.
type row struct{ short, full, note string }

type section struct {
	title, intro string
	cols         [3]string
	rows         []row
}

// ReferenceCommand returns "kdeck alias reference": every shortcut kdeck knows,
// with the full command it stands for. The output is built from the command
// tree and the shortcut grammar, so it cannot drift from the code;
// docs/SHORTCUTS.md is its --markdown output.
func ReferenceCommand(out io.Writer) *cobra.Command {
	var markdown bool
	cmd := &cobra.Command{
		Use:     "reference [REGEX]",
		Aliases: []string{"ref", "docs"},
		Short:   "Every shortcut and alias with the full command it stands for (--markdown for docs/SHORTCUTS.md)",
		Example: `  kdeck alias reference              # everything, grouped
  kdeck alias reference 'logs'       # only rows mentioning logs
  kdeck alias reference --markdown > docs/SHORTCUTS.md`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := ""
			if len(args) == 1 {
				pattern = args[0]
			}
			re, err := match.Compile(pattern)
			if err != nil {
				return err
			}
			sections := Reference(cmd.Root(), !markdown)
			for i := range sections {
				var keep []row
				for _, r := range sections[i].rows {
					if re.Any(r.short, r.full, r.note) {
						keep = append(keep, r)
					}
				}
				sections[i].rows = keep
			}
			if markdown {
				return writeMarkdown(out, sections)
			}
			return writeText(out, sections)
		},
	}
	cmd.Flags().BoolVar(&markdown, "markdown", false, "write Markdown (built-in shortcuts only, without your own aliases)")
	return cmd
}

// Reference collects every shortcut, grouped. withUser adds the aliases in
// the user's config.yaml.
func Reference(root *cobra.Command, withUser bool) []section {
	var secs []section

	if withUser {
		if cfg, err := config.Load(); err == nil && len(cfg.Aliases) > 0 {
			s := section{title: "Your aliases", intro: "From config.yaml (kdeck alias set). They win over every built-in shortcut.", cols: [3]string{"Alias", "Full command", ""}}
			for _, k := range sortedNames(cfg.Aliases) {
				s.rows = append(s.rows, row{k, fullForm(root, alias.SplitWords(cfg.Aliases[k])), ""})
			}
			secs = append(secs, s)
		}
	}

	shellSec := section{
		title: "Shell aliases",
		intro: "Defined by `eval \"$(kdeck shell-init zsh)\"` (or bash, fish, powershell, cmd). `kdeck shortcuts install` creates the same names as real commands, so they also work in scripts.",
		cols:  [3]string{"Alias", "Full command", "Notes"},
		rows:  []row{{"k", "kdeck", "kdeck itself; --name changes it"}, {"kh CONTEXT", "kdeck here CONTEXT", "switch context for this terminal only; kh --off to return"}},
	}
	for _, h := range ShellHelpers {
		shellSec.rows = append(shellSec.rows, row{h[0], h[1], ""})
	}
	secs = append(secs, shellSec)

	dk := map[string]bool{}
	for _, n := range quick.DKNames {
		dk[n] = true
	}
	cmdSec := section{
		title: "Command aliases",
		intro: "Other names for kdeck and kubectl commands. Names starting with dk (dk = deck) are also shell aliases and real commands, so `dklog api` works on its own.",
		cols:  [3]string{"Alias", "Full command", "What it does"},
	}
	var walk func(c *cobra.Command, kdeckTree bool)
	walk = func(c *cobra.Command, kdeckTree bool) {
		for _, sub := range c.Commands() {
			if !sub.IsAvailableCommand() || strings.Contains(sub.GroupID, "plugin") {
				continue
			}
			mine := kdeckTree || sub.GroupID == "kdeck" || sub.GroupID == "shortcuts"
			path := strings.TrimPrefix(sub.CommandPath(), root.Name()+" ")
			prog := "kubectl"
			if mine {
				prog = "kdeck"
			}
			parent := strings.TrimPrefix(strings.TrimPrefix(c.CommandPath(), root.Name()), " ")
			for _, a := range sub.Aliases {
				note := sub.Short
				if dk[a] {
					note += " (also a standalone command)"
				}
				short := a
				if parent != "" {
					short = parent + " " + a
				}
				cmdSec.rows = append(cmdSec.rows, row{short, prog + " " + path, note})
			}
			walk(sub, mine)
		}
	}
	walk(root, false)
	sort.SliceStable(cmdSec.rows, func(i, j int) bool { return cmdSec.rows[i].short < cmdSec.rows[j].short })
	secs = append(secs, cmdSec)

	verbSec := section{
		title: "Verb shortcuts",
		intro: "One word for a common command. Use them after kdeck (`kdeck rr deploy/api`) or as shell aliases.",
		cols:  [3]string{"Shortcut", "Full command", ""},
	}
	for _, k := range sortedNames(alias.Verbs) {
		verbSec.rows = append(verbSec.rows, row{k, alias.FullForm(alias.Verbs[k]), ""})
	}
	secs = append(secs, verbSec)

	gram := section{
		title: "Combination grammar",
		intro: "Words follow `[k][sys]<cmd>[<resource>][<modifiers>]`, as in kubectl-aliases. The leading k is optional after kdeck (`kdeck gpo` = `kdeck kgpo`). `sys` adds `--namespace=kube-system`. A value modifier (l, n, f) takes the next argument: `kgpol app=web` = `kubectl get pods -l app=web`. A word that is a real command is never expanded.",
		cols:  [3]string{"Code", "Expands to", "Notes"},
	}
	for _, p := range alias.GrammarCommands() {
		gram.rows = append(gram.rows, row{"cmd " + p.Code, p.Expands, p.Notes})
	}
	for _, p := range alias.GrammarResources() {
		gram.rows = append(gram.rows, row{"resource " + p.Code, p.Expands, p.Notes})
	}
	for _, p := range alias.GrammarModifiers() {
		gram.rows = append(gram.rows, row{"modifier " + p.Code, p.Expands, p.Notes})
	}
	secs = append(secs, gram)

	// Every valid word, one section per command so the tables stay readable.
	byVerb := map[string]*section{}
	var order []string
	for _, w := range alias.All() {
		exp, _ := alias.ParseCombo(w)
		key := exp[0]
		if strings.HasPrefix(w, "ksys") {
			key += " (kube-system)"
		}
		s, ok := byVerb[key]
		if !ok {
			s = &section{title: "Combinations: " + key, cols: [3]string{"Shortcut", "Full command", "Also"}}
			byVerb[key] = s
			order = append(order, key)
		}
		also := ""
		if short := strings.TrimSuffix(w, "all") + "a"; strings.HasSuffix(w, "all") {
			if e, ok := alias.ParseCombo(short); ok && strings.Join(e, " ") == strings.Join(exp, " ") {
				also = short
			}
		}
		s.rows = append(s.rows, row{w, alias.FullForm(exp), also})
	}
	rank := map[string]int{"get": 0, "describe": 1, "delete": 2, "logs": 3, "exec": 4, "port-forward": 5, "apply": 6, "proxy": 7}
	sort.SliceStable(order, func(i, j int) bool {
		si, sj := strings.HasSuffix(order[i], ")"), strings.HasSuffix(order[j], ")")
		if si != sj {
			return !si
		}
		return rank[strings.Fields(order[i])[0]] < rank[strings.Fields(order[j])[0]]
	})
	for _, k := range order {
		secs = append(secs, *byVerb[k])
	}
	return secs
}

func sortedNames[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeText(out io.Writer, secs []section) error {
	for _, s := range secs {
		if len(s.rows) == 0 {
			continue
		}
		fmt.Fprintf(out, "%s\n%s\n", strings.ToUpper(s.title), strings.Repeat("=", len(s.title)))
		if s.intro != "" {
			fmt.Fprintln(out, strings.ReplaceAll(s.intro, "`", ""))
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, r := range s.rows {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.short, r.full, r.note)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(out)
	}
	return nil
}

func writeMarkdown(out io.Writer, secs []section) error {
	total := 0
	for _, s := range secs {
		total += len(s.rows)
	}
	fmt.Fprintf(out, `# kdeck shortcut reference

Every shortcut and alias kdeck understands, with the full command it stands for (%d entries).
Generated by `+"`kdeck alias reference --markdown`"+`; do not edit by hand (`+"`make docs`"+` regenerates it).

Look one up from the terminal:

- `+"`kdeck alias explain kgpooyaml`"+`: what a word expands to
- `+"`kdeck help kgpo`"+`: the expansion, then the help of the command it runs
- `+"`kdeck alias reference logs`"+`: this reference, filtered by a regex
- `+"`kdeck alias set NAME 'EXPANSION'`"+`: add your own; yours win over everything below

## Contents

`, total)
	for _, s := range secs {
		if len(s.rows) > 0 {
			fmt.Fprintf(out, "- [%s](#%s) (%d)\n", s.title, anchor(s.title), len(s.rows))
		}
	}
	for _, s := range secs {
		if len(s.rows) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n## %s\n\n", s.title)
		if s.intro != "" {
			fmt.Fprintf(out, "%s\n\n", s.intro)
		}
		cols := s.cols
		if cols[2] == "" {
			fmt.Fprintf(out, "| %s | %s |\n| --- | --- |\n", cols[0], cols[1])
		} else {
			fmt.Fprintf(out, "| %s | %s | %s |\n| --- | --- | --- |\n", cols[0], cols[1], cols[2])
		}
		for _, r := range s.rows {
			if cols[2] == "" {
				fmt.Fprintf(out, "| `%s` | `%s` |\n", r.short, r.full)
			} else {
				fmt.Fprintf(out, "| `%s` | `%s` | %s |\n", r.short, r.full, mdEscape(r.note))
			}
		}
	}
	return nil
}

// anchor is GitHub's heading slug: lower case, spaces to dashes, most
// punctuation dropped.
func anchor(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

func mdEscape(s string) string {
	return strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;").Replace(s)
}
