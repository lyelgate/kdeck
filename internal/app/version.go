package app

import "runtime/debug"

// kubectlVersion reports the k8s.io/kubectl module version compiled in.
func kubectlVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, d := range info.Deps {
		if d.Path == "k8s.io/kubectl" {
			return d.Version
		}
	}
	return "unknown"
}
