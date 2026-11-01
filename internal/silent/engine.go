package silent

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/lyelgate/kubepilot/internal/config"
	"github.com/lyelgate/kubepilot/internal/match"
)

// AnnotationIgnore suppresses findings on the annotated object, e.g.
// "SF-PDB-001; until=2027-01-31; reason=single replica by design".
const AnnotationIgnore = "silent.lyelgate.github.io/ignore"

// Options select what a run does.
type Options struct {
	Checks         []string // IDs or categories; empty = all non-sensitive
	Skip           []string
	Sensitive      bool
	Since          time.Duration // only findings that began within this window
	IncludeUnknown bool
	MinSeverity    Severity
	Match          *match.Matcher // regex over "namespace/Kind/name", summary and check ID
	Now            time.Time
	Config         config.SilentConfig
}

// Selected returns the checks a run will execute, and why others are skipped.
func Selected(o Options) ([]Check, []Skipped) {
	want := func(c Check, list []string) bool {
		for _, w := range list {
			w = strings.TrimSpace(w)
			if strings.EqualFold(w, c.ID) || strings.EqualFold(w, c.Category) {
				return true
			}
		}
		return false
	}
	var run []Check
	var skipped []Skipped
	for _, c := range Registry {
		if len(o.Skip) > 0 && want(c, o.Skip) {
			continue
		}
		if cc, ok := o.Config.Checks[c.ID]; ok && cc.Enabled != nil && !*cc.Enabled {
			continue
		}
		explicit := len(o.Checks) > 0 && want(c, o.Checks)
		if len(o.Checks) > 0 && !explicit {
			continue
		}
		if c.Sensitive && !o.Sensitive {
			skipped = append(skipped, Skipped{c.ID, "reads Secrets; enable with --sensitive"})
			continue
		}
		run = append(run, c)
	}
	return run, skipped
}

// Run executes the selected checks against a snapshot.
func Run(ctx context.Context, s *Snapshot, o Options) Result {
	start := time.Now()
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	p := &Params{Now: o.Now, thresholds: map[string]map[string]float64{}, backupSel: o.Config.BackupLabels}
	for id, cc := range o.Config.Checks {
		p.thresholds[id] = cc.Thresholds
	}
	checks, skipped := Selected(o)
	var findings []Finding
	for _, c := range checks {
		fs, err := runOne(ctx, c, s, p)
		if err != nil {
			skipped = append(skipped, Skipped{c.ID, reason(err)})
			continue
		}
		findings = append(findings, fs...)
	}
	var kept []Finding
	for _, f := range findings {
		f.fingerprint()
		if excludedNamespace(f.Namespace, o.Config.ExcludeNamespaces) {
			continue
		}
		f.Suppressed = suppressed(f, o.Config.Suppressions, o.Now)
		if f.Severity < o.MinSeverity {
			continue
		}
		if !o.Match.Any(f.Object(), f.Summary, f.Check) {
			continue
		}
		if o.Since > 0 {
			if f.Since == nil && !o.IncludeUnknown {
				continue
			}
			if f.Since != nil && o.Now.Sub(*f.Since) > o.Since {
				continue
			}
		}
		kept = append(kept, f)
	}
	Sort(kept)
	return Result{Findings: kept, Skipped: skipped, Duration: time.Since(start).Round(time.Millisecond).String()}
}

func runOne(ctx context.Context, c Check, s *Snapshot, p *Params) (fs []Finding, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("check panicked: %v", r)
		}
	}()
	return c.Run(ctx, s, p)
}

func reason(err error) string {
	switch {
	case err == errNotInstalled, apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return "API not installed in this cluster"
	case apierrors.IsForbidden(err):
		return "forbidden: " + firstLine(err.Error())
	}
	return firstLine(err.Error())
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func excludedNamespace(ns string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, ns); ok && ns != "" {
			return true
		}
	}
	return false
}

// suppressed returns the reason when a config entry or annotation hides f.
// An expired suppression stops applying.
func suppressed(f Finding, list []config.Suppression, now time.Time) string {
	for _, s := range list {
		if !strings.EqualFold(s.Check, f.Check) && s.Check != "*" {
			continue
		}
		if ok, _ := path.Match(s.Object, f.Object()); !ok && s.Object != f.Fingerprint {
			continue
		}
		if expired(s.Until, now) {
			continue
		}
		return s.Reason
	}
	if v := f.annotations[AnnotationIgnore]; v != "" {
		parts := strings.Split(v, ";")
		ids := strings.Split(parts[0], ",")
		match := false
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "*" || strings.EqualFold(id, f.Check) {
				match = true
			}
		}
		until, why := "", "annotation "+AnnotationIgnore
		for _, kv := range parts[1:] {
			k, val, _ := strings.Cut(strings.TrimSpace(kv), "=")
			switch k {
			case "until":
				until = val
			case "reason":
				why = val
			}
		}
		if match && !expired(until, now) {
			return why
		}
	}
	return ""
}

func expired(until string, now time.Time) bool {
	if until == "" {
		return false
	}
	t, err := time.Parse("2006-01-02", until)
	return err == nil && now.After(t.Add(24*time.Hour))
}

// Sort orders findings by severity, then oldest first, unknown ages last.
func Sort(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		switch {
		case a.Since != nil && b.Since != nil:
			if !a.Since.Equal(*b.Since) {
				return a.Since.Before(*b.Since)
			}
		case a.Since != nil:
			return true
		case b.Since != nil:
			return false
		}
		return a.Object() < b.Object()
	})
}

// ParseDuration accepts Go durations plus days, e.g. "36h", "7d", "1d12h".
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var total time.Duration
	if i := strings.Index(s, "d"); i > 0 {
		var days float64
		if _, err := fmt.Sscanf(s[:i], "%g", &days); err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total += Days(days)
		s = s[i+1:]
	}
	if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, err
		}
		total += d
	}
	return total, nil
}
