// Package kubeconfig merges kubeconfig files into the user's kubeconfig
// safely: a timestamped backup first, and incoming entries renamed so a merge
// never overwrites an existing cluster, user or context.
package kubeconfig

import (
	"fmt"
	"os"
	"sort"
	"time"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// Path returns the kubeconfig file kdeck writes to (first entry of KUBECONFIG,
// --kubeconfig, or ~/.kube/config).
func Path(flags *genericclioptions.ConfigFlags) string {
	return flags.ToRawKubeConfigLoader().ConfigAccess().GetDefaultFilename()
}

// Backup copies the kubeconfig to <file>.kdeck-backup-<timestamp>; returns ""
// when there was nothing to back up.
func Backup(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	backup := fmt.Sprintf("%s.kdeck-backup-%s", path, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return "", fmt.Errorf("writing backup: %w", err)
	}
	return backup, nil
}

// Merge adds raw (a kubeconfig document) under the context name: as its own
// file in the store (the default), or merged into the user's kubeconfig after
// a backup when the mode is "merge" or --kubeconfig names a file. It returns
// the file written and the backup ("" when none was needed).
func Merge(flags *genericclioptions.ConfigFlags, raw []byte, name string, setCurrent bool) (string, string, error) {
	if Separate() && (flags.KubeConfig == nil || *flags.KubeConfig == "") {
		path, err := Store(raw, name)
		if err != nil {
			return "", "", err
		}
		if setCurrent {
			err = SetCurrent(name)
		}
		return path, "", err
	}
	incoming, err := clientcmd.Load(raw)
	if err != nil {
		return "", "", fmt.Errorf("parsing kubeconfig: %w", err)
	}
	access := flags.ToRawKubeConfigLoader().ConfigAccess()
	path := access.GetDefaultFilename()
	existing, err := access.GetStartingConfig()
	if err != nil {
		return "", "", err
	}
	backup, err := Backup(path)
	if err != nil {
		return "", "", err
	}
	merged := MergeConfigs(existing, incoming, name, setCurrent)
	if err := clientcmd.ModifyConfig(access, *merged, true); err != nil {
		return "", "", err
	}
	return path, backup, nil
}

// MergeConfigs is the pure part of Merge. Each incoming context becomes
// name (then name-2, name-3, ...) with its cluster renamed to match and its
// user renamed to <name>-admin.
func MergeConfigs(existing, incoming *api.Config, name string, setCurrent bool) *api.Config {
	out := existing.DeepCopy()
	if out.Clusters == nil {
		out.Clusters = map[string]*api.Cluster{}
	}
	if out.AuthInfos == nil {
		out.AuthInfos = map[string]*api.AuthInfo{}
	}
	if out.Contexts == nil {
		out.Contexts = map[string]*api.Context{}
	}
	ctxNames := make([]string, 0, len(incoming.Contexts))
	for n := range incoming.Contexts {
		ctxNames = append(ctxNames, n)
	}
	sort.Strings(ctxNames)
	// The incoming current context, if any, takes the plain name.
	if c := incoming.CurrentContext; c != "" {
		for i, n := range ctxNames {
			if n == c {
				ctxNames[0], ctxNames[i] = ctxNames[i], ctxNames[0]
			}
		}
	}
	for i, n := range ctxNames {
		c := incoming.Contexts[n]
		target := name
		if i > 0 {
			target = fmt.Sprintf("%s-%d", name, i+1)
		}
		cluster, user := target, target+"-admin"
		if cl, ok := incoming.Clusters[c.Cluster]; ok {
			out.Clusters[cluster] = cl.DeepCopy()
		}
		if au, ok := incoming.AuthInfos[c.AuthInfo]; ok {
			out.AuthInfos[user] = au.DeepCopy()
		}
		out.Contexts[target] = &api.Context{Cluster: cluster, AuthInfo: user, Namespace: c.Namespace}
		if setCurrent && i == 0 {
			out.CurrentContext = target
		}
	}
	return out
}
