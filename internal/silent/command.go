package silent

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/lyelgate/kubepilot/internal/config"
	"github.com/lyelgate/kubepilot/internal/match"
)

// ExitError carries a process exit code without printing an error.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// NewCommand returns "kp silent".
func NewCommand(flags *genericclioptions.ConfigFlags, out io.Writer, version string) *cobra.Command {
	var (
		checks, skip                       []string
		sensitive, includeUnknown, explain bool
		showSuppressed, list               bool
		output, sinceStr, minSev, failOn   string
		matchRe                            string
	)
	cmd := &cobra.Command{
		Use:     "silent",
		Aliases: []string{"sf", "health"},
		Short:   "Find failures that produce no crash and no alert (backups, certs, leases, webhooks, ...)",
		Long: `Find things that stopped working without anyone being told: backups that
stopped succeeding, certificates about to expire, controllers that lost their
leader lease, webhooks and Services with nothing behind them, HPAs frozen
because metrics are down, PDBs that will block every drain.

Every finding says what broke, since when (from the API, never guessed),
the evidence, and what to do next. Read-only; safe on production clusters.`,
		Example: `  kp silent                         # all namespaces, all non-sensitive checks
  kp silent -n payments             # one namespace (cluster-scoped checks still run)
  kp silent --check cert,backup     # by category or check ID
  kp silent --sensitive             # also read Secrets for certificate expiry
  kp silent --since 24h             # only failures that started in the last day
  kp silent -o markdown > report.md # shareable report
  kp silent -o json --fail-on high  # CI: exit 1 on high or critical
  kp silent --list                  # show the check catalogue
  kp silent -m 'payments|billing'   # regex over object, summary and check ID`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if list {
				return printCatalogue(out)
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			since, err := ParseDuration(sinceStr)
			if err != nil {
				return err
			}
			minSeverity, err := ParseSeverity(minSev)
			if err != nil {
				return err
			}
			failAt, err := ParseSeverity(failOn)
			if err != nil {
				return err
			}
			m, err := match.Compile(matchRe)
			if err != nil {
				return err
			}
			rc, err := flags.ToRESTConfig()
			if err != nil {
				return err
			}
			rc.QPS, rc.Burst = 20, 40
			cs, err := kubernetes.NewForConfig(rc)
			if err != nil {
				return err
			}
			dyn, err := dynamic.NewForConfig(rc)
			if err != nil {
				return err
			}
			ns := ""
			if flags.Namespace != nil && *flags.Namespace != "" {
				ns = *flags.Namespace
			}
			raw, _ := flags.ToRawKubeConfigLoader().RawConfig()
			cluster := raw.CurrentContext
			if flags.Context != nil && *flags.Context != "" {
				cluster = *flags.Context
			}
			now := time.Now()
			res := Run(cmd.Context(), NewSnapshot(cs, dyn, ns), Options{
				Checks: splitList(checks), Skip: splitList(skip), Sensitive: sensitive,
				Since: since, IncludeUnknown: includeUnknown, MinSeverity: minSeverity,
				Now: now, Config: cfg.Silent, Match: m,
			})
			res.Cluster, res.Version = cluster, version
			if err := Write(out, res, output, now, showSuppressed); err != nil {
				return err
			}
			if explain && len(res.Skipped) > 0 && output != "json" {
				fmt.Fprintln(out, "\nSkipped checks:")
				for _, s := range res.Skipped {
					fmt.Fprintf(out, "  %s: %s\n", s.Check, s.Reason)
				}
			}
			for _, f := range res.Findings {
				if f.Suppressed == "" && f.Severity >= failAt {
					return ExitError{Code: 1}
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&checks, "check", nil, "check IDs or categories to run (backup, certificate, rbac, control, webhook, network, workload, storage)")
	f.StringSliceVar(&skip, "skip", nil, "check IDs or categories to skip")
	f.BoolVar(&sensitive, "sensitive", false, "enable checks that read Secrets (certificate expiry)")
	f.StringVarP(&output, "output", "o", "table", "table, wide, json or markdown")
	f.StringVar(&sinceStr, "since", "", "only findings that began within this window (24h, 7d)")
	f.BoolVar(&includeUnknown, "include-unknown", false, "with --since, keep findings whose start time is unknown")
	f.StringVar(&minSev, "min-severity", "low", "hide findings below this severity")
	f.StringVar(&failOn, "fail-on", "critical", "exit 1 when a finding is at or above this severity (none to disable)")
	f.BoolVar(&explain, "explain-skipped", false, "list checks that could not run and why")
	f.BoolVar(&showSuppressed, "show-suppressed", false, "include suppressed findings")
	f.BoolVar(&list, "list", false, "print the check catalogue and exit")
	f.StringVarP(&matchRe, "match", "m", "", "only findings whose object, summary or check ID matches this regex")
	return cmd
}

func splitList(in []string) []string {
	var out []string
	for _, s := range in {
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func printCatalogue(out io.Writer) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCATEGORY\tSENSITIVE\tTITLE")
	for _, c := range Registry {
		s := ""
		if c.Sensitive {
			s = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.ID, c.Category, s, c.Title)
	}
	return tw.Flush()
}
