// Package silent finds Kubernetes failures that produce no crash and no
// default alert: backups that stopped succeeding, certificates about to
// expire, controllers that lost their lease, Services with nothing behind
// them. Every finding says what broke, since when, and what to do.
//
// The engine is read-only. Checks are pure functions of a Snapshot, which
// lists each resource at most once per run and shares it between checks.
package silent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Severity of a finding: impact if nobody acts.
type Severity int

const (
	Low Severity = iota
	Medium
	High
	Critical
)

func (s Severity) String() string {
	return [...]string{"low", "medium", "high", "critical"}[s]
}

// ParseSeverity accepts low, medium, high or critical.
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToLower(s) {
	case "low":
		return Low, nil
	case "medium":
		return Medium, nil
	case "high":
		return High, nil
	case "critical":
		return Critical, nil
	case "none", "never":
		return Critical + 1, nil
	}
	return Low, fmt.Errorf("unknown severity %q (low, medium, high, critical, none)", s)
}

// MarshalText makes severities print as words in JSON.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Finding is one silent failure on one object.
type Finding struct {
	Check       string     `json:"check"`
	Title       string     `json:"title"`
	Severity    Severity   `json:"severity"`
	Kind        string     `json:"kind"`
	Namespace   string     `json:"namespace,omitempty"`
	Name        string     `json:"name"`
	Summary     string     `json:"summary"`
	Since       *time.Time `json:"since,omitempty"`
	SinceSource string     `json:"sinceSource,omitempty"`
	Evidence    []string   `json:"evidence,omitempty"`
	Remediation string     `json:"remediation"`
	Fingerprint string     `json:"fingerprint"`
	Suppressed  string     `json:"suppressed,omitempty"` // reason, when suppressed

	annotations map[string]string
	detailKey   string
}

// Object renders "namespace/Kind/name" or "Kind/name".
func (f Finding) Object() string {
	if f.Namespace == "" {
		return f.Kind + "/" + f.Name
	}
	return f.Namespace + "/" + f.Kind + "/" + f.Name
}

func (f *Finding) fingerprint() {
	h := sha256.Sum256([]byte(f.Check + "|" + f.Object() + "|" + f.detailKey))
	f.Fingerprint = hex.EncodeToString(h[:8])
}

// Check is one detector.
type Check struct {
	ID        string
	Title     string
	Category  string
	Sensitive bool // reads Secrets or the kubelet API; off unless --sensitive
	Run       func(ctx context.Context, s *Snapshot, p *Params) ([]Finding, error)
}

// Params carries the clock and configurable thresholds into checks.
type Params struct {
	Now        time.Time
	thresholds map[string]map[string]float64
	backupSel  []string
}

// T returns a threshold for a check, or def when not configured.
func (p *Params) T(check, key string, def float64) float64 {
	if m, ok := p.thresholds[check]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return def
}

// Days returns a duration of n days.
func Days(n float64) time.Duration { return time.Duration(n * 24 * float64(time.Hour)) }

// Skipped records a check that could not run.
type Skipped struct {
	Check  string `json:"check"`
	Reason string `json:"reason"`
}

// Result is the outcome of one run.
type Result struct {
	Cluster  string    `json:"cluster"`
	Findings []Finding `json:"findings"`
	Skipped  []Skipped `json:"skipped,omitempty"`
	Duration string    `json:"duration"`
	Version  string    `json:"version"`
}

func tp(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
