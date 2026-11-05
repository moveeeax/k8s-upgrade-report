// Package rules holds the checked-in dataset of Kubernetes API removals and
// managed-addon version floors, plus the minor-version arithmetic used to
// decide whether a removal matters for a given upgrade.
package rules

import (
	_ "embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed data/deprecations.yaml
var deprecationsYAML []byte

//go:embed data/addons.yaml
var addonsYAML []byte

// Transform names the mechanical rewrite that turns the old object into the new one.
type Transform string

const (
	// TransformAPIVersion means rewriting apiVersion is enough.
	TransformAPIVersion Transform = "apiversion"
	// TransformIngressV1 means apiVersion plus backend/path field moves.
	TransformIngressV1 Transform = "ingress-v1"
	// TransformHPAV2 means apiVersion plus metric target restructuring.
	TransformHPAV2 Transform = "hpa-v2"
	// TransformManual means no mechanical rewrite exists.
	TransformManual Transform = "manual"
)

// Rule is one removed API.
type Rule struct {
	ID           string    `yaml:"id" json:"id"`
	Kind         string    `yaml:"kind" json:"kind"`
	From         string    `yaml:"from" json:"from"`
	To           string    `yaml:"to" json:"to"`
	DeprecatedIn string    `yaml:"deprecatedIn" json:"deprecatedIn"`
	RemovedIn    string    `yaml:"removedIn" json:"removedIn"`
	Transform    Transform `yaml:"transform" json:"transform"`
	Docs         string    `yaml:"docs" json:"docs"`
	Notes        string    `yaml:"notes" json:"notes,omitempty"`
}

// Autofixable reports whether this rule can be patched without a human decision.
func (r Rule) Autofixable() bool { return r.Transform != TransformManual && r.To != "" }

type ruleFile struct {
	Version int    `yaml:"version"`
	Rules   []Rule `yaml:"rules"`
}

// Provider is one managed Kubernetes offering.
type Provider struct {
	Name       string                       `yaml:"name" json:"name"`
	Sequencing []string                     `yaml:"sequencing" json:"sequencing"`
	Addons     map[string]map[string]string `yaml:"addons" json:"addons"`
}

type addonFile struct {
	Version   int                 `yaml:"version"`
	Providers map[string]Provider `yaml:"providers"`
}

// Set is the loaded dataset.
type Set struct {
	rules     []Rule
	byGVK     map[string]Rule
	providers map[string]Provider
}

// Load parses the embedded dataset. It fails loudly on a malformed or
// self-contradictory dataset so that a bad edit is caught by the tests rather
// than by a client's report.
func Load() (*Set, error) {
	var rf ruleFile
	if err := yaml.Unmarshal(deprecationsYAML, &rf); err != nil {
		return nil, fmt.Errorf("parse deprecations.yaml: %w", err)
	}
	var af addonFile
	if err := yaml.Unmarshal(addonsYAML, &af); err != nil {
		return nil, fmt.Errorf("parse addons.yaml: %w", err)
	}

	s := &Set{byGVK: make(map[string]Rule, len(rf.Rules)), providers: af.Providers}
	seen := map[string]bool{}
	for _, r := range rf.Rules {
		switch {
		case r.ID == "":
			return nil, fmt.Errorf("rule for %s/%s has no id", r.From, r.Kind)
		case seen[r.ID]:
			return nil, fmt.Errorf("duplicate rule id %q", r.ID)
		case r.Kind == "" || r.From == "":
			return nil, fmt.Errorf("rule %q: kind and from are required", r.ID)
		case r.Transform == "":
			return nil, fmt.Errorf("rule %q: transform is required", r.ID)
		}
		if _, err := ParseMinor(r.RemovedIn); err != nil {
			return nil, fmt.Errorf("rule %q: removedIn: %w", r.ID, err)
		}
		if _, err := ParseMinor(r.DeprecatedIn); err != nil {
			return nil, fmt.Errorf("rule %q: deprecatedIn: %w", r.ID, err)
		}
		key := gvkKey(r.From, r.Kind)
		if _, dup := s.byGVK[key]; dup {
			return nil, fmt.Errorf("rule %q: %s already covered by another rule", r.ID, key)
		}
		seen[r.ID] = true
		s.byGVK[key] = r
		s.rules = append(s.rules, r)
	}
	sort.Slice(s.rules, func(i, j int) bool {
		a, b := s.rules[i], s.rules[j]
		if a.RemovedIn != b.RemovedIn {
			return MinorLess(mustMinor(a.RemovedIn), mustMinor(b.RemovedIn))
		}
		return a.ID < b.ID
	})
	return s, nil
}

// Rules returns every rule, ordered by removal version.
func (s *Set) Rules() []Rule { return append([]Rule(nil), s.rules...) }

// Lookup finds the rule for an apiVersion/kind pair, if any.
func (s *Set) Lookup(apiVersion, kind string) (Rule, bool) {
	r, ok := s.byGVK[gvkKey(apiVersion, kind)]
	return r, ok
}

// Provider returns the addon dataset for a cloud, keyed by lowercase name.
func (s *Set) Provider(name string) (Provider, bool) {
	p, ok := s.providers[strings.ToLower(name)]
	return p, ok
}

// ProviderNames lists the supported clouds.
func (s *Set) ProviderNames() []string {
	out := make([]string, 0, len(s.providers))
	for k := range s.providers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func gvkKey(apiVersion, kind string) string { return apiVersion + "|" + kind }

// Minor is a Kubernetes minor version such as 1.29.
type Minor struct {
	Major int
	Minor int
}

func (m Minor) String() string { return fmt.Sprintf("%d.%d", m.Major, m.Minor) }

// Next returns the next minor version.
func (m Minor) Next() Minor { return Minor{Major: m.Major, Minor: m.Minor + 1} }

// ParseMinor accepts "1.29", "v1.29" or "1.29.4" and keeps major.minor only.
func ParseMinor(s string) (Minor, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "v"))
	if s == "" {
		return Minor{}, fmt.Errorf("empty version")
	}
	// Drop anything after a patch separator or suffix such as "-eks-1234".
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return Minor{}, fmt.Errorf("version %q is not major.minor", s)
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return Minor{}, fmt.Errorf("version %q: bad major: %w", s, err)
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return Minor{}, fmt.Errorf("version %q: bad minor: %w", s, err)
	}
	return Minor{Major: maj, Minor: min}, nil
}

func mustMinor(s string) Minor {
	m, err := ParseMinor(s)
	if err != nil {
		panic(err)
	}
	return m
}

// MinorLess reports whether a is older than b.
func MinorLess(a, b Minor) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	return a.Minor < b.Minor
}

// MinorLessOrEqual reports whether a is not newer than b.
func MinorLessOrEqual(a, b Minor) bool { return a == b || MinorLess(a, b) }

// Path returns every minor version strictly after from and up to to.
func Path(from, to Minor) []Minor {
	var out []Minor
	for cur := from.Next(); MinorLessOrEqual(cur, to); cur = cur.Next() {
		out = append(out, cur)
		if len(out) > 40 { // guard against a nonsense range
			break
		}
	}
	return out
}
