package silent

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"k8s.io/apimachinery/pkg/util/duration"
)

// Write renders a result in the given format: table, wide, json or markdown.
func Write(w io.Writer, r Result, format string, now time.Time, showSuppressed bool) error {
	visible, suppressedCount := split(r.Findings, showSuppressed)
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	case "markdown", "md":
		return writeMarkdown(w, r, visible, suppressedCount, now)
	case "wide":
		return writeTable(w, r, visible, suppressedCount, now, true)
	case "table", "":
		return writeTable(w, r, visible, suppressedCount, now, false)
	}
	return fmt.Errorf("unknown output format %q (table, wide, json, markdown)", format)
}

func split(fs []Finding, showSuppressed bool) ([]Finding, int) {
	var out []Finding
	n := 0
	for _, f := range fs {
		if f.Suppressed != "" {
			n++
			if !showSuppressed {
				continue
			}
		}
		out = append(out, f)
	}
	return out, n
}

func since(f Finding, now time.Time) string {
	if f.Since == nil {
		return "unknown"
	}
	if f.Since.After(now) {
		return "in " + duration.HumanDuration(f.Since.Sub(now))
	}
	return duration.HumanDuration(now.Sub(*f.Since)) + " ago"
}

// clean removes control characters so object names from the cluster cannot
// inject terminal escape sequences.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func counts(fs []Finding) string {
	c := map[Severity]int{}
	for _, f := range fs {
		c[f.Severity]++
	}
	var parts []string
	for _, s := range []Severity{Critical, High, Medium, Low} {
		if c[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c[s], s))
		}
	}
	return strings.Join(parts, ", ")
}

func writeTable(w io.Writer, r Result, fs []Finding, suppressedCount int, now time.Time, wide bool) error {
	if len(fs) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		if wide {
			fmt.Fprintln(tw, "SEVERITY\tCHECK\tOBJECT\tSINCE\tSUMMARY\tEVIDENCE\tNEXT STEP")
		} else {
			fmt.Fprintln(tw, "SEVERITY\tCHECK\tOBJECT\tSINCE\tSUMMARY")
		}
		for _, f := range fs {
			sev := f.Severity.String()
			if f.Suppressed != "" {
				sev += " (suppressed)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s", sev, f.Check, clean(f.Object()), since(f, now), clean(f.Summary))
			if wide {
				fmt.Fprintf(tw, "\t%s\t%s", clean(strings.Join(f.Evidence, "; ")), clean(f.Remediation))
			}
			fmt.Fprintln(tw)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	summary := fmt.Sprintf("%d finding", len(fs))
	if len(fs) != 1 {
		summary += "s"
	}
	if c := counts(fs); c != "" {
		summary += " (" + c + ")"
	}
	if len(fs) == 0 {
		summary = "No silent failures found"
	}
	summary += " in " + r.Duration
	if suppressedCount > 0 {
		summary += fmt.Sprintf(" · %d suppressed", suppressedCount)
	}
	if len(r.Skipped) > 0 {
		summary += fmt.Sprintf(" · %d skipped (--explain-skipped)", len(r.Skipped))
	}
	fmt.Fprintln(w, summary)
	return nil
}

func writeMarkdown(w io.Writer, r Result, fs []Finding, suppressedCount int, now time.Time) error {
	esc := func(s string) string {
		return strings.NewReplacer("|", "\\|", "[", "\\[", "]", "\\]", "<", "&lt;").Replace(clean(s))
	}
	fmt.Fprintf(w, "# Silent failure report\n\n")
	if r.Cluster != "" {
		fmt.Fprintf(w, "Cluster: `%s` · ", esc(r.Cluster))
	}
	fmt.Fprintf(w, "Generated %s · %d findings", now.UTC().Format("2006-01-02 15:04 UTC"), len(fs))
	if c := counts(fs); c != "" {
		fmt.Fprintf(w, " (%s)", c)
	}
	fmt.Fprintf(w, "\n\n")
	for _, sev := range []Severity{Critical, High, Medium, Low} {
		var rows []Finding
		for _, f := range fs {
			if f.Severity == sev {
				rows = append(rows, f)
			}
		}
		if len(rows) == 0 {
			continue
		}
		title := strings.ToUpper(sev.String()[:1]) + sev.String()[1:]
		fmt.Fprintf(w, "## %s (%d)\n\n| Check | Object | Since | Summary | Next step |\n| --- | --- | --- | --- | --- |\n", title, len(rows))
		for _, f := range rows {
			fmt.Fprintf(w, "| %s | `%s` | %s | %s | %s |\n", f.Check, esc(f.Object()), since(f, now), esc(f.Summary), esc(f.Remediation))
		}
		fmt.Fprintln(w)
	}
	if suppressedCount > 0 {
		fmt.Fprintf(w, "%d findings suppressed by configuration or annotations.\n\n", suppressedCount)
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintf(w, "## Skipped checks\n\n")
		for _, s := range r.Skipped {
			fmt.Fprintf(w, "- %s: %s\n", s.Check, esc(s.Reason))
		}
	}
	return nil
}
