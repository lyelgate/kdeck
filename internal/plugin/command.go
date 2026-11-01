package plugin

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// NewCommand returns "kdeck plugin", replacing kubectl's own "plugin" command.
// It keeps "kdeck plugin list" (installed plugins on PATH) and adds krew's
// install, search, info, upgrade, uninstall and update.
func NewCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "plugin",
		Aliases: []string{"plugins", "krew", "pl"},
		Short:   "Install and manage kubectl plugins from the krew index (krew-compatible)",
		Long: `Install and manage kubectl plugins from the krew index, without installing krew.

Plugins land in ~/.kdeck and run as "kdeck <name>" (or "kubectl <name>" if you add
~/.kdeck/bin to PATH). Plugins already installed with krew keep working.
Every download is checked against the sha256 in the index.`,
		Example: `  kdeck plugin update              # refresh the index
  kdeck plugin search secret
  kdeck plugin info view-secret
  kdeck plugin install view-secret tree neat
  kdeck view-secret db-creds        # run it
  kdeck plugin list                 # installed plugins
  kdeck plugin upgrade              # upgrade everything installed by kdeck
  kdeck plugin uninstall tree`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "update",
			Short: "Download the latest krew index",
			RunE: func(cmd *cobra.Command, _ []string) error {
				n, err := UpdateIndex()
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "Index updated: %d plugins available.\n", n)
				return nil
			},
		},
		&cobra.Command{
			Use:     "search [REGEX]",
			Aliases: []string{"find"},
			Short:   "Search plugins by name or description (regex, smart case)",
			Args:    cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				q := ""
				if len(args) == 1 {
					q = args[0]
				}
				ms, err := Search(q)
				if err != nil {
					return err
				}
				installed := installedSet()
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tINSTALLED\tDESCRIPTION")
				for _, m := range ms {
					mark := "no"
					if installed[m.Metadata.Name] {
						mark = "yes"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", m.Metadata.Name, mark, m.Spec.ShortDescription)
				}
				return tw.Flush()
			},
		},
		&cobra.Command{
			Use:   "info NAME",
			Short: "Show a plugin's version, homepage, platforms and caveats",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				m, err := Load(args[0])
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "Name:        %s\nVersion:     %s\nHomepage:    %s\n", m.Metadata.Name, m.Spec.Version, m.Spec.Homepage)
				var plats []string
				supported := false
				for _, p := range m.Spec.Platforms {
					plats = append(plats, platformLabel(p))
					if p.Matches(runtime.GOOS, runtime.GOARCH) {
						supported = true
					}
				}
				sort.Strings(plats)
				fmt.Fprintf(out, "Platforms:   %s\nThis system: %s/%s (supported: %v)\n\n%s\n", strings.Join(plats, ", "), runtime.GOOS, runtime.GOARCH, supported, strings.TrimSpace(m.Spec.Description))
				if m.Spec.Caveats != "" {
					fmt.Fprintf(out, "\nCaveats:\n%s\n", strings.TrimSpace(m.Spec.Caveats))
				}
				return nil
			},
		},
		&cobra.Command{
			Use:     "install NAME...",
			Aliases: []string{"add", "i"},
			Short:   "Install plugins",
			Args:    cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				var failed []string
				for _, name := range args {
					m, err := Install(name, out)
					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", name, err)
						failed = append(failed, name)
						continue
					}
					fmt.Fprintf(out, "Installed %s %s. Run it with: kdeck %s\n", name, m.Spec.Version, name)
					if m.Spec.Caveats != "" {
						fmt.Fprintf(out, "Caveats:\n%s\n", strings.TrimSpace(m.Spec.Caveats))
					}
				}
				if len(failed) > 0 {
					return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
				}
				return nil
			},
		},
		&cobra.Command{
			Use:     "uninstall NAME...",
			Aliases: []string{"remove", "rm"},
			Short:   "Uninstall plugins installed by kdeck",
			Args:    cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				for _, name := range args {
					if err := Uninstall(name); err != nil {
						return err
					}
					fmt.Fprintf(out, "Uninstalled %s.\n", name)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "upgrade [NAME...]",
			Short: "Upgrade plugins installed by kdeck (all when no name is given)",
			RunE: func(cmd *cobra.Command, args []string) error {
				if _, err := UpdateIndex(); err != nil {
					return err
				}
				recs, err := Installed()
				if err != nil {
					return err
				}
				want := map[string]bool{}
				for _, a := range args {
					want[a] = true
				}
				for _, r := range recs {
					if len(want) > 0 && !want[r.Name] {
						continue
					}
					m, err := Load(r.Name)
					if err != nil {
						fmt.Fprintf(out, "%s: %v\n", r.Name, err)
						continue
					}
					if m.Spec.Version == r.Version {
						fmt.Fprintf(out, "%s is up to date (%s).\n", r.Name, r.Version)
						continue
					}
					if _, err := Install(r.Name, out); err != nil {
						fmt.Fprintf(out, "%s: %v\n", r.Name, err)
						continue
					}
					fmt.Fprintf(out, "Upgraded %s %s -> %s.\n", r.Name, r.Version, m.Spec.Version)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List installed plugins: kdeck-managed, krew-managed and any kubectl-* on PATH",
			RunE: func(cmd *cobra.Command, _ []string) error {
				recs, _ := Installed()
				tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "PLUGIN\tVERSION\tSOURCE\tPATH")
				seen := map[string]bool{}
				for _, r := range recs {
					fmt.Fprintf(tw, "%s\t%s\tkp\t%s\n", r.Name, r.Version, filepath.Join(binDir(), BinName(r.Name)))
					seen[BinName(r.Name)] = true
				}
				for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
					entries, err := os.ReadDir(dir)
					if err != nil {
						continue
					}
					for _, e := range entries {
						n := e.Name()
						if !strings.HasPrefix(n, "kubectl-") || seen[n] {
							continue
						}
						seen[n] = true
						source := "PATH"
						if strings.Contains(dir, ".krew") {
							source = "krew"
						}
						name := strings.ReplaceAll(strings.TrimPrefix(strings.TrimSuffix(n, ".exe"), "kubectl-"), "_", "-")
						fmt.Fprintf(tw, "%s\t-\t%s\t%s\n", name, source, filepath.Join(dir, n))
					}
				}
				return tw.Flush()
			},
		},
	)
	return cmd
}

func installedSet() map[string]bool {
	set := map[string]bool{}
	recs, _ := Installed()
	for _, r := range recs {
		set[r.Name] = true
	}
	return set
}

func platformLabel(p Platform) string {
	osName, arch := p.Selector.MatchLabels["os"], p.Selector.MatchLabels["arch"]
	switch {
	case osName != "" && arch != "":
		return osName + "/" + arch
	case osName != "":
		return osName + "/*"
	}
	return "custom selector"
}
