// Package match gives every kdeck search the same regex behaviour.
//
// Patterns are Go regular expressions (RE2). Matching is "smart case", like
// ripgrep: case-insensitive unless the pattern contains an upper-case
// letter. An empty pattern matches everything.
package match

import (
	"fmt"
	"regexp"
	"unicode"
)

// Matcher tests strings against a compiled pattern.
type Matcher struct {
	re  *regexp.Regexp
	raw string
}

// Compile builds a smart-case matcher.
func Compile(pattern string) (*Matcher, error) {
	if pattern == "" {
		return &Matcher{}, nil
	}
	expr := pattern
	if !hasUpper(pattern) {
		expr = "(?i)" + pattern
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid regex %q: %w", pattern, err)
	}
	return &Matcher{re: re, raw: pattern}, nil
}

// MustCompile is Compile for patterns known to be valid.
func MustCompile(pattern string) *Matcher {
	m, err := Compile(pattern)
	if err != nil {
		panic(err)
	}
	return m
}

// Match reports whether s matches. A nil or empty matcher matches anything.
func (m *Matcher) Match(s string) bool {
	if m == nil || m.re == nil {
		return true
	}
	return m.re.MatchString(s)
}

// Any reports whether any of the strings matches.
func (m *Matcher) Any(ss ...string) bool {
	if m == nil || m.re == nil {
		return true
	}
	for _, s := range ss {
		if m.re.MatchString(s) {
			return true
		}
	}
	return false
}

// Empty reports whether the matcher matches everything.
func (m *Matcher) Empty() bool { return m == nil || m.re == nil }

// String returns the pattern as given.
func (m *Matcher) String() string {
	if m == nil {
		return ""
	}
	return m.raw
}

func hasUpper(s string) bool {
	escaped := false
	for _, r := range s {
		if escaped {
			escaped = false
			continue // \S, \W, \D are classes, not upper-case letters
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}
