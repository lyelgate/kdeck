// Package alias expands short words into full kubectl arguments.
//
// Three kinds of shortcut are understood, checked in this order:
//
//  1. User aliases from config.yaml ("kdeck alias set kgr 'get pods -o wide'").
//
//  2. Verb aliases: one word for a common command, such as "l" for "logs"
//     or "rr" for "rollout restart".
//
//  3. Combinations following the kubectl-aliases grammar
//     (github.com/ahmetb/kubectl-aliases):
//
//     [k][sys]<cmd>[<resource>][<modifiers>...]
//
//     kgpo        get pods
//     kgpoa       get pods --all-namespaces      ("all" is also accepted)
//     kgpooyaml   get pods -o yaml
//     kgdepw      get deployment --watch
//     ksysgpo     get pods --namespace=kube-system
//     kdpol app=x describe pods -l app=x
//
// A word that names a real command is never expanded.
package alias

import (
	"fmt"
	"sort"
	"strings"
)

// Verbs maps one-word shortcuts to their expansion.
var Verbs = map[string][]string{
	"l":    {"logs"},
	"lf":   {"logs", "-f"},
	"lp":   {"logs", "-p"},
	"e":    {"exec", "-it"},
	"ed":   {"edit"},
	"c":    {"create"},
	"ak":   {"apply", "-k"},
	"df":   {"diff", "-f"},
	"ro":   {"rollout"},
	"rr":   {"rollout", "restart"},
	"rs":   {"rollout", "status"},
	"rh":   {"rollout", "history"},
	"ru":   {"rollout", "undo"},
	"sc":   {"scale"},
	"t":    {"top"},
	"tp":   {"top", "pods"},
	"tn":   {"top", "nodes"},
	"x":    {"explain"},
	"ar":   {"api-resources"},
	"lb":   {"label"},
	"an":   {"annotate"},
	"dr":   {"drain", "--ignore-daemonsets", "--delete-emptydir-data"},
	"co":   {"cordon"},
	"uc":   {"uncordon"},
	"ta":   {"taint"},
	"ev":   {"events"},
	"evw":  {"events", "--types=Warning"},
	"ci":   {"cluster-info"},
	"v":    {"version"},
	"cfg":  {"config"},
	"w":    {"wait"},
	"dbg":  {"debug"},
	"can":  {"auth", "can-i"},
	"canl": {"auth", "can-i", "--list"},
	// Teleport plugin.
	"tl":  {"tsh", "login"},
	"tls": {"tsh", "ls"},
	"tu":  {"tsh", "use"},
	"ts":  {"tsh", "status"},
	"tlo": {"tsh", "logout"},
}

// VerbHelp describes each verb alias, for "kdeck alias list".
var VerbHelp = map[string]string{}

func init() {
	for k, v := range Verbs {
		VerbHelp[k] = strings.Join(v, " ")
	}
}

type cmdDef struct {
	expand      []string
	takesRes    bool
	allowedMods map[string]bool
}

// Commands of the kubectl-aliases grammar.
var cmds = map[string]cmdDef{
	"g":   {[]string{"get"}, true, set("oyaml", "ojson", "owide", "all", "a", "w", "sl", "f", "l", "n")},
	"d":   {[]string{"describe"}, true, set("all", "a", "f", "l", "n")},
	"rm":  {[]string{"delete"}, true, set("all", "a", "f", "l", "n")},
	"a":   {[]string{"apply", "-f"}, false, set()},
	"ex":  {[]string{"exec", "-i", "-t"}, false, set("n")},
	"lo":  {[]string{"logs", "-f"}, false, set("n")},
	"lop": {[]string{"logs", "-f", "-p"}, false, set("n")},
	"p":   {[]string{"proxy"}, false, set()},
	"pf":  {[]string{"port-forward"}, false, set("n")},
}

// Resources maps short codes to kubectl resource names.
var Resources = map[string]string{
	"po":    "pods",
	"dep":   "deployment",
	"sts":   "statefulset",
	"ds":    "daemonset",
	"rs":    "replicaset",
	"svc":   "service",
	"ing":   "ingress",
	"cm":    "configmap",
	"sec":   "secret",
	"no":    "nodes",
	"ns":    "namespaces",
	"job":   "job",
	"cj":    "cronjob",
	"pvc":   "persistentvolumeclaim",
	"pv":    "persistentvolume",
	"hpa":   "horizontalpodautoscaler",
	"sa":    "serviceaccount",
	"ev":    "events",
	"ep":    "endpointslices",
	"pdb":   "poddisruptionbudget",
	"np":    "networkpolicy",
	"crd":   "customresourcedefinition",
	"sc":    "storageclass",
	"lease": "lease",
}

// Modifiers and the flags they add. Value modifiers (f, l, n) take the
// user's next argument as their value, so at most one is allowed.
var modifiers = map[string][]string{
	"oyaml": {"-o", "yaml"},
	"ojson": {"-o", "json"},
	"owide": {"-o", "wide"},
	"all":   {"--all-namespaces"},
	"a":     {"--all-namespaces"},
	"w":     {"--watch"},
	"sl":    {"--show-labels"},
	"f":     {"--recursive", "-f"},
	"l":     {"-l"},
	"n":     {"--namespace"},
}

var valueMods = set("f", "l", "n")
var outputMods = set("oyaml", "ojson", "owide")

func set(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}

// byLengthDesc returns map keys, longest first, for greedy matching.
func byLengthDesc[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

// ParseCombo expands one kubectl-aliases word. It returns false when the
// word does not follow the grammar.
func ParseCombo(word string) ([]string, bool) {
	s := strings.TrimPrefix(word, "k")
	if s == "" {
		return nil, false
	}
	var out []string
	sys := false
	if strings.HasPrefix(s, "sys") {
		sys = true
		s = s[3:]
	}

	// Command: longest match first ("lop" before "lo").
	var cmd cmdDef
	found := false
	for _, k := range byLengthDesc(cmds) {
		if strings.HasPrefix(s, k) {
			cmd, found = cmds[k], true
			s = s[len(k):]
			break
		}
	}
	if !found {
		return nil, false
	}
	out = append(out, cmd.expand...)

	// Optional resource.
	if cmd.takesRes {
		for _, k := range byLengthDesc(Resources) {
			if strings.HasPrefix(s, k) {
				out = append(out, Resources[k])
				s = s[len(k):]
				break
			}
		}
	}

	// Modifiers, in any order, each at most once.
	seen := map[string]bool{}
	var value []string
	for s != "" {
		matched := false
		for _, k := range byLengthDesc(modifiers) {
			if !strings.HasPrefix(s, k) || !cmd.allowedMods[k] {
				continue
			}
			norm := k
			if k == "a" {
				norm = "all"
			}
			if seen[norm] {
				return nil, false
			}
			seen[norm] = true
			if valueMods[k] {
				if value != nil {
					return nil, false // two value modifiers are ambiguous
				}
				value = modifiers[k]
			} else {
				out = append(out, modifiers[k]...)
			}
			s = s[len(k):]
			matched = true
			break
		}
		if !matched {
			return nil, false
		}
	}

	// Incompatible combinations.
	outputs := 0
	for k := range outputMods {
		if seen[k] {
			outputs++
		}
	}
	if outputs > 1 || (seen["w"] && (seen["oyaml"] || seen["ojson"])) {
		return nil, false
	}
	if seen["all"] && (sys || seen["n"]) {
		return nil, false
	}
	if sys {
		out = append(out, "--namespace=kube-system")
	}
	out = append(out, value...)
	return out, true
}

// Expand rewrites args when the first non-flag word is a shortcut.
// isCommand reports whether a word is a real command, which is never expanded.
// It returns the new args and the shortcut used ("" when none).
func Expand(args []string, user map[string]string, isCommand func(string) bool) ([]string, string) {
	// The first word that is neither a flag nor the value of a global flag
	// ("-n prod kgpo" -> "kgpo").
	idx := -1
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if i > 0 && takesValue(args[i-1]) {
			continue
		}
		idx = i
		break
	}
	if idx < 0 {
		return args, ""
	}

	word := args[idx]
	var exp []string
	switch {
	case user[word] != "":
		exp = SplitWords(user[word])
	case isCommand != nil && isCommand(word):
		return args, ""
	case Verbs[word] != nil:
		exp = Verbs[word]
	default:
		c, ok := ParseCombo(word)
		if !ok {
			return args, ""
		}
		exp = c
	}
	out := make([]string, 0, len(args)+len(exp))
	out = append(out, args[:idx]...)
	out = append(out, exp...)
	out = append(out, args[idx+1:]...)
	return out, word
}

func takesValue(flag string) bool {
	switch flag {
	case "-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user", "-s", "--server", "--token", "--as", "--as-group", "--request-timeout":
		return true
	}
	return false
}

// SplitWords splits a string into words, honouring single and double quotes.
func SplitWords(s string) []string {
	var words []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// Curated is the set of combinations "kdeck shell-init" turns into shell aliases
// by default. "--all" generates every valid combination instead.
var Curated = []string{
	"kgpo", "kgpoa", "kgpoowide", "kgpooyaml", "kgpow", "kgpol", "kgpon", "kgposl",
	"kgdep", "kgdepa", "kgdepoyaml", "kgdepw", "kgsts", "kgds", "kgrs",
	"kgsvc", "kgsvca", "kgsvcoyaml", "kging", "kginga", "kgcm", "kgcmoyaml", "kgsec", "kgsecoyaml",
	"kgno", "kgnoowide", "kgnosl", "kgns", "kgjob", "kgcj", "kgpvc", "kgpvca", "kgpv", "kghpa", "kgev", "kgeva",
	"kdpo", "kddep", "kdsvc", "kding", "kdno", "kdcm", "kdsec", "kdpvc",
	"krmpo", "krmdep", "krmsvc", "krmcm", "krmsec", "krmpvc",
	"ka", "kex", "klo", "klop", "kpf",
	"ksysgpo", "ksysgpoowide", "ksysgdep", "ksysgsvc", "ksysdpo", "ksyslo",
}

// All enumerates every valid combination word, for "kdeck shell-init --all".
func All() []string {
	var words []string
	cmdKeys := byLengthDesc(cmds)
	sort.Strings(cmdKeys)
	resKeys := append([]string{""}, sortedKeys(Resources)...)
	modOrder := []string{"oyaml", "ojson", "owide", "all", "w", "sl", "f", "l", "n"}
	for _, sys := range []string{"", "sys"} {
		for _, c := range cmdKeys {
			def := cmds[c]
			resList := []string{""}
			if def.takesRes {
				resList = resKeys
			}
			for _, r := range resList {
				for _, m := range append([]string{""}, modOrder...) {
					if m != "" && !def.allowedMods[m] {
						continue
					}
					w := "k" + sys + c + r + m
					if exp, ok := ParseCombo(w); ok && len(exp) > 0 {
						words = append(words, w)
					}
				}
			}
		}
	}
	return words
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Describe returns "word -> expansion" for display.
func Describe(word string) string {
	if exp, ok := ParseCombo(word); ok {
		return fmt.Sprintf("%-14s %s", word, FullForm(exp))
	}
	return word
}

// kdeckVerbs are expansions that start with a kdeck command, not a kubectl one.
var kdeckVerbs = set("tsh")

// FullForm renders an expansion as the command a user would type without the
// shortcut: "kubectl get pods -l <value>", or "kdeck tsh login" for kdeck's
// own commands. A trailing value flag gets a <value> placeholder.
func FullForm(exp []string) string {
	if len(exp) == 0 {
		return ""
	}
	prog := "kubectl"
	if kdeckVerbs[exp[0]] {
		prog = "kdeck"
	}
	s := prog + " " + strings.Join(exp, " ")
	switch exp[len(exp)-1] {
	case "-l", "--namespace", "-f":
		s += " <value>"
	}
	return s
}

// Part is one element of the combination grammar and what it adds.
type Part struct {
	Code, Expands, Notes string
}

// GrammarCommands lists the <cmd> codes in a fixed order, with the
// modifiers each accepts.
func GrammarCommands() []Part {
	order := []string{"g", "d", "rm", "a", "ex", "lo", "lop", "p", "pf"}
	modOrder := []string{"oyaml", "ojson", "owide", "a", "all", "w", "sl", "l", "n", "f"}
	parts := make([]Part, 0, len(order))
	for _, k := range order {
		def := cmds[k]
		var mods []string
		for _, m := range modOrder {
			if def.allowedMods[m] {
				mods = append(mods, m)
			}
		}
		notes := "no resource"
		if def.takesRes {
			notes = "takes a resource"
		}
		if len(mods) > 0 {
			notes += "; modifiers: " + strings.Join(mods, " ")
		}
		parts = append(parts, Part{k, strings.Join(def.expand, " "), notes})
	}
	return parts
}

// GrammarResources lists the <resource> codes alphabetically.
func GrammarResources() []Part {
	var parts []Part
	for _, k := range sortedKeys(Resources) {
		parts = append(parts, Part{Code: k, Expands: Resources[k]})
	}
	return parts
}

// GrammarModifiers lists the <modifier> codes in a fixed order.
func GrammarModifiers() []Part {
	notes := map[string]string{
		"a": "same as all", "all": "cannot be combined with sys or n",
		"w": "not with oyaml or ojson", "l": "takes the next argument", "n": "takes the next argument", "f": "takes the next argument",
	}
	var parts []Part
	for _, k := range []string{"oyaml", "ojson", "owide", "a", "all", "w", "sl", "l", "n", "f"} {
		parts = append(parts, Part{k, strings.Join(modifiers[k], " "), notes[k]})
	}
	return parts
}
