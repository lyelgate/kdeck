package workspace

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/lyelgate/kubepilot/internal/config"
)

// SettingsCommand returns "kp settings": export and import kp's own
// configuration (aliases, safety rules, profiles, Teleport proxy, silent
// tuning, cost prices) to carry it to another machine. kp stores no tokens,
// so the export contains no credentials.
func SettingsCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Show, export and import kp's settings (aliases, safety, profiles, Teleport, silent, cost)",
		Example: `  kp settings path
  kp settings export > kp-settings.yaml
  kp settings import kp-settings.yaml           # merge into yours
  kp settings import kp-settings.yaml --replace`,
	}
	cmd.AddCommand(&cobra.Command{
		Use: "path", Short: "Print the settings file location",
		Run: func(cmd *cobra.Command, _ []string) { fmt.Fprintln(out, config.File()) },
	})
	cmd.AddCommand(&cobra.Command{
		Use: "export", Short: "Print settings as YAML",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			data, err := yaml.Marshal(cfg)
			if err != nil {
				return err
			}
			_, err = out.Write(data)
			return err
		},
	})
	var replace bool
	imp := &cobra.Command{
		Use: "import FILE", Short: "Merge settings from a file (or replace with --replace)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var in config.Config
			if err := yaml.UnmarshalStrict(data, &in); err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			cfg := &in
			if !replace {
				cur, err := config.Load()
				if err != nil {
					return err
				}
				cfg = Merge(cur, &in)
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(out, "Imported settings into %s.\n", config.File())
			return nil
		},
	}
	imp.Flags().BoolVar(&replace, "replace", false, "replace instead of merging")
	cmd.AddCommand(imp)
	return cmd
}

// Merge overlays in onto cur: map entries and non-empty values from in win,
// pattern lists are unioned.
func Merge(cur, in *config.Config) *config.Config {
	out := *cur
	if out.Aliases == nil {
		out.Aliases = map[string]string{}
	}
	for k, v := range in.Aliases {
		out.Aliases[k] = v
	}
	if out.Profiles == nil {
		out.Profiles = map[string]config.Profile{}
	}
	for k, v := range in.Profiles {
		out.Profiles[k] = v
	}
	out.Safety.Production = union(out.Safety.Production, in.Safety.Production)
	out.Safety.ReadOnly = union(out.Safety.ReadOnly, in.Safety.ReadOnly)
	if in.Safety.DiffBeforeApply != nil {
		out.Safety.DiffBeforeApply = in.Safety.DiffBeforeApply
	}
	if in.Teleport.Proxy != "" {
		out.Teleport = in.Teleport
	}
	if in.Linode.DefaultRegion != "" {
		out.Linode = in.Linode
	}
	if in.Update.Repo != "" {
		out.Update = in.Update
	}
	if out.Silent.Checks == nil {
		out.Silent.Checks = map[string]config.CheckConfig{}
	}
	for k, v := range in.Silent.Checks {
		out.Silent.Checks[k] = v
	}
	out.Silent.ExcludeNamespaces = union(out.Silent.ExcludeNamespaces, in.Silent.ExcludeNamespaces)
	out.Silent.BackupLabels = union(out.Silent.BackupLabels, in.Silent.BackupLabels)
	out.Silent.Suppressions = append(out.Silent.Suppressions, in.Silent.Suppressions...)
	if out.Cost.NodePrices == nil {
		out.Cost.NodePrices = map[string]float64{}
	}
	for k, v := range in.Cost.NodePrices {
		out.Cost.NodePrices[k] = v
	}
	if in.Cost.DefaultNodePrice > 0 {
		out.Cost.DefaultNodePrice = in.Cost.DefaultNodePrice
	}
	if in.Cost.StoragePerGB > 0 {
		out.Cost.StoragePerGB = in.Cost.StoragePerGB
	}
	if in.Cost.Currency != "" {
		out.Cost.Currency = in.Cost.Currency
	}
	return &out
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
