package quick

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"

	"github.com/lyelgate/kdeck/internal/match"
	"github.com/lyelgate/kdeck/internal/stern"
)

// onePod resolves a regex to a single pod: an exact name wins; otherwise the
// best match (running and ready first). With several matches the picker
// opens on a terminal with fzf; otherwise the best one is used and the
// others are mentioned on stderr.
func onePod(d Deps, cmd *cobra.Command, pattern string, all bool) (corev1.Pod, error) {
	pods, err := matchingPods(d, cmd, pattern, all, "")
	if err != nil {
		return corev1.Pod{}, err
	}
	if len(pods) == 0 {
		return corev1.Pod{}, fmt.Errorf("no pod matches %q (pod names use lower-case letters, digits, '-' and '.')", pattern)
	}
	for _, p := range pods {
		if p.Name == pattern {
			return p, nil
		}
	}
	if len(pods) == 1 {
		return pods[0], nil
	}
	names := make([]string, len(pods))
	byName := map[string]corev1.Pod{}
	for i, p := range pods {
		names[i] = p.Namespace + "/" + p.Name
		byName[names[i]] = p
	}
	if os.Getenv("KDECK_NO_FZF") == "" && isTTY(os.Stdin) && isTTY(os.Stdout) {
		if choice, err := pickOne("pod", names); err == nil {
			return byName[choice], nil
		}
	}
	fmt.Fprintf(d.ErrOut, "%d pods match %q; using %s (others: use --all or a tighter regex)\n", len(pods), pattern, names[0])
	return pods[0], nil
}

func singleFlags(cmd *cobra.Command, all, every *bool) {
	cmd.Flags().BoolVarP(all, "all-namespaces", "A", false, "search all namespaces")
	cmd.Flags().BoolVar(every, "all", false, "act on every matching pod, not just one")
}

// logCmd: logs of one pod (or all matches with --all), by regex.
func logCmd(d Deps) *cobra.Command {
	var allNS, every, follow, previous, timestamps bool
	var container, since string
	var tail int64
	cmd := &cobra.Command{
		Use:     "log POD-REGEX",
		Aliases: []string{"dklog", "dkl"},
		Short:   "Logs of the pod matching a regex (one pod; --all for every match, stern-style)",
		Long:    "Show logs of one pod chosen by regex: exact name first, then the best running match.\n" + regexHelp,
		Example: `  kdeck log abc               # abc-7d9f-x2, abc-worker-1, ... -> the best match
  kdeck log abc -f            # follow
  kdeck log abc -c sidecar    # a container (regex)
  kdeck log abc -p            # previous (crashed) container
  kdeck log abc --all -f      # every matching pod, colour-prefixed
  dklog abc                # same, after "kdeck shortcuts install"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if every {
				cs, ns, err := d.client()
				if err != nil {
					return err
				}
				if allNS {
					ns = ""
				}
				pm, err := match.Compile(args[0])
				if err != nil {
					return err
				}
				cm, err := match.Compile(container)
				if err != nil {
					return err
				}
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				return stern.Run(ctx, cs, stern.Options{Namespace: ns, Pod: pm, Container: cm, Tail: tail, Follow: follow,
					Timestamps: timestamps, Color: isTTY(os.Stdout) && os.Getenv("NO_COLOR") == ""}, d.Out)
			}
			p, err := onePod(d, cmd, args[0], allNS)
			if err != nil {
				return err
			}
			kargs := []string{"logs", p.Name, "-n", p.Namespace, fmt.Sprintf("--tail=%d", tail)}
			if container != "" {
				cm, err := match.Compile(container)
				if err != nil {
					return err
				}
				var picked []string
				for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
					if cm.Match(c.Name) {
						picked = append(picked, c.Name)
					}
				}
				if len(picked) == 0 {
					return fmt.Errorf("no container in %s matches %q", p.Name, container)
				}
				kargs = append(kargs, "-c", picked[0])
			} else if len(p.Spec.Containers) > 1 {
				kargs = append(kargs, "--all-containers", "--prefix")
			}
			if follow {
				kargs = append(kargs, "-f")
			}
			if previous {
				kargs = append(kargs, "-p")
			}
			if timestamps {
				kargs = append(kargs, "--timestamps")
			}
			if since != "" {
				kargs = append(kargs, "--since="+since)
			}
			return d.Kubectl(kargs...)
		},
	}
	singleFlags(cmd, &allNS, &every)
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow")
	cmd.Flags().BoolVarP(&previous, "previous", "p", false, "logs of the previous (crashed) container")
	cmd.Flags().BoolVarP(&timestamps, "timestamps", "t", false, "timestamps")
	cmd.Flags().StringVarP(&container, "container", "c", "", "container name regex (default: all containers)")
	cmd.Flags().StringVar(&since, "since", "", "only logs newer than this (10m, 2h)")
	cmd.Flags().Int64Var(&tail, "tail", 200, "lines of history (-1 = all)")
	return cmd
}

// descCmd: kubectl describe for one pod (or all matches).
func descCmd(d Deps) *cobra.Command {
	var allNS, every bool
	cmd := &cobra.Command{
		Use:     "desc POD-REGEX",
		Aliases: []string{"dkdesc", "dkd"},
		Short:   "kubectl describe of the pod matching a regex (one pod; --all for every match)",
		Long:    "Describe one pod chosen by regex.\n" + regexHelp,
		Example: `  kdeck desc abc
  kdeck desc abc --all
  dkdesc abc`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pods, err := targets(d, cmd, args[0], allNS, every)
			if err != nil {
				return err
			}
			for _, p := range pods {
				if err := d.Kubectl("describe", "pod", p.Name, "-n", p.Namespace); err != nil {
					return err
				}
			}
			return nil
		},
	}
	singleFlags(cmd, &allNS, &every)
	return cmd
}

// imgCmd: images (and resolved digests) of one pod.
func imgCmd(d Deps) *cobra.Command {
	var allNS, every bool
	cmd := &cobra.Command{
		Use:     "img POD-REGEX",
		Aliases: []string{"dkimg", "dki"},
		Short:   "Images of the pod matching a regex: container, image, resolved digest",
		Long:    "List each container's image and the digest actually running.\n" + regexHelp,
		Example: `  kdeck img abc
  kdeck img abc --all
  dkimg abc`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pods, err := targets(d, cmd, args[0], allNS, every)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(d.Out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "POD\tCONTAINER\tIMAGE\tRUNNING DIGEST")
			for _, p := range pods {
				ids := map[string]string{}
				for _, s := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
					ids[s.Name] = s.ImageID
				}
				for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, c.Name, c.Image, dash(ids[c.Name]))
				}
			}
			return tw.Flush()
		},
	}
	singleFlags(cmd, &allNS, &every)
	return cmd
}

// poCmd: kubectl get for the matching pods (all matches; listing is the point).
func poCmd(d Deps) *cobra.Command {
	var allNS bool
	var output string
	cmd := &cobra.Command{
		Use:     "po POD-REGEX",
		Aliases: []string{"dkpo", "getpo"},
		Short:   "kubectl get pods for every pod matching a regex (-o wide by default)",
		Long:    "Run kubectl get on the pods whose names match a regex.\n" + regexHelp,
		Example: `  kdeck po abc
  kdeck po abc -o yaml
  kdeck po '^abc-' -A
  dkpo abc`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pods, err := matchingPods(d, cmd, args[0], allNS, "")
			if err != nil {
				return err
			}
			if len(pods) == 0 {
				return fmt.Errorf("no pod matches %q", args[0])
			}
			byNS := map[string][]string{}
			var order []string
			for _, p := range pods {
				if _, ok := byNS[p.Namespace]; !ok {
					order = append(order, p.Namespace)
				}
				byNS[p.Namespace] = append(byNS[p.Namespace], p.Name)
			}
			for _, ns := range order {
				kargs := append([]string{"get", "pods", "-n", ns, "-o", output}, byNS[ns]...)
				if err := d.Kubectl(kargs...); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&allNS, "all-namespaces", "A", false, "search all namespaces")
	cmd.Flags().StringVarP(&output, "output", "o", "wide", "kubectl output format (wide, yaml, json, name, ...)")
	return cmd
}

// targets is one pod, or every match with --all.
func targets(d Deps, cmd *cobra.Command, pattern string, allNS, every bool) ([]corev1.Pod, error) {
	if every {
		pods, err := matchingPods(d, cmd, pattern, allNS, "")
		if err != nil {
			return nil, err
		}
		if len(pods) == 0 {
			return nil, fmt.Errorf("no pod matches %q", pattern)
		}
		return pods, nil
	}
	p, err := onePod(d, cmd, pattern, allNS)
	if err != nil {
		return nil, err
	}
	return []corev1.Pod{p}, nil
}

// DKNames are the dk-prefixed command names "kdeck shortcuts install" links, so
// "dklog abc" works straight from the shell.
var DKNames = []string{"dklog", "dkdesc", "dkimg", "dkpo", "dkpod", "dksh", "dkpf", "dktail", "dkns", "dkctx", "dkfind", "dkbad"}
