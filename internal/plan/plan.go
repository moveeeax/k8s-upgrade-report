// Package plan turns a scan result into the part of the report the free tools
// do not produce: the hop-by-hop sequence, the addon floors for the cloud the
// cluster runs on, and a plain-English go/no-go with named blockers.
package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/moveeeax/k8s-upgrade-report/internal/rules"
	"github.com/moveeeax/k8s-upgrade-report/internal/scan"
)

// Decision is the go/no-go call.
type Decision string

const (
	// DecisionGo means nothing found stops the upgrade.
	DecisionGo Decision = "go"
	// DecisionGoWithPatches means every blocker is mechanically fixable first.
	DecisionGoWithPatches Decision = "go-after-patches"
	// DecisionNoGo means at least one blocker needs a human decision or a vendor.
	DecisionNoGo Decision = "no-go"
)

// Removal is one API that stops being served at a given hop.
type Removal struct {
	Rule rules.Rule `json:"rule"`
	Hits int        `json:"hits"` // resources in the scanned manifests using it
}

// Hop is one minor-version step of the upgrade.
type Hop struct {
	From        string            `json:"from"`
	To          string            `json:"to"`
	Removals    []Removal         `json:"removals"`
	AddonFloors map[string]string `json:"addonFloors,omitempty"`
	Hits        int               `json:"hits"`
}

// Verdict is the go/no-go plus the reasons behind it.
type Verdict struct {
	Decision   Decision `json:"decision"`
	Summary    string   `json:"summary"`
	Blockers   []string `json:"blockers,omitempty"`
	OutOfScope []string `json:"outOfScope,omitempty"`
}

// Plan is the whole sequencing section of the report.
type Plan struct {
	Current      string   `json:"current"`
	Target       string   `json:"target"`
	Provider     string   `json:"provider,omitempty"`
	ProviderName string   `json:"providerName,omitempty"`
	Hops         []Hop    `json:"hops"`
	Sequencing   []string `json:"sequencing,omitempty"`
	Verdict      Verdict  `json:"verdict"`
}

// Options configures plan generation.
type Options struct {
	Current  rules.Minor
	Target   rules.Minor
	Provider string // eks, aks, gke, or empty
}

// These are quoted separately, per the service scope. Naming them in the
// report is the point: an unnamed exclusion turns into an argument later.
var outOfScope = []string{
	"CRD schema migration and operator upgrades",
	"Service mesh version jumps (Istio, Linkerd)",
	"Anything outside EKS, AKS and GKE",
	"Applying changes to a live cluster - this tool only generates patches",
}

// Build assembles the sequencing plan and the verdict.
func Build(set *rules.Set, res scan.Result, opts Options) Plan {
	p := Plan{Current: opts.Current.String(), Target: opts.Target.String()}

	if opts.Provider != "" {
		if prov, ok := set.Provider(opts.Provider); ok {
			p.Provider = strings.ToLower(opts.Provider)
			p.ProviderName = prov.Name
			p.Sequencing = prov.Sequencing
		}
	}

	hits := map[string]int{}
	for _, f := range res.Findings {
		if f.Category == scan.CategoryAPIRemoval {
			hits[f.RuleID]++
		}
	}

	for _, to := range rules.Path(opts.Current, opts.Target) {
		hop := Hop{From: prevOf(to).String(), To: to.String()}
		for _, r := range set.Rules() {
			rm, err := rules.ParseMinor(r.RemovedIn)
			if err != nil || rm != to {
				continue
			}
			n := hits[r.ID]
			if n == 0 {
				continue // only list removals this cluster actually trips over
			}
			hop.Removals = append(hop.Removals, Removal{Rule: r, Hits: n})
			hop.Hits += n
		}
		sort.Slice(hop.Removals, func(i, j int) bool { return hop.Removals[i].Rule.ID < hop.Removals[j].Rule.ID })
		if p.Provider != "" {
			prov, _ := set.Provider(p.Provider)
			hop.AddonFloors = prov.Addons[to.String()]
		}
		p.Hops = append(p.Hops, hop)
	}

	p.Verdict = verdict(res)
	p.Verdict.OutOfScope = outOfScope
	return p
}

func prevOf(m rules.Minor) rules.Minor {
	if m.Minor == 0 {
		return m
	}
	return rules.Minor{Major: m.Major, Minor: m.Minor - 1}
}

func verdict(res scan.Result) Verdict {
	var manual, vendor, auto []scan.Finding
	for _, f := range res.Findings {
		if f.Severity != scan.SeverityBlocker {
			continue
		}
		switch {
		case f.Owner != nil:
			vendor = append(vendor, f)
		case f.Autofix:
			auto = append(auto, f)
		default:
			manual = append(manual, f)
		}
	}

	v := Verdict{}
	for _, f := range vendor {
		v.Blockers = append(v.Blockers, fmt.Sprintf("%s in %s is owned by %s - %s",
			f.Resource.Kind, f.Resource.File, ownerLabel(f.Owner), f.Owner.Action()))
	}
	for _, f := range manual {
		v.Blockers = append(v.Blockers, fmt.Sprintf("%s %s (%s) needs a hand-written change: %s",
			f.Resource.Kind, f.Resource.Name, f.Resource.File, f.Remediation))
	}

	switch {
	case len(vendor) == 0 && len(manual) == 0 && len(auto) == 0:
		v.Decision = DecisionGo
		v.Summary = "Nothing in the scanned manifests blocks this upgrade. Sequence the control plane and node pools as described below."
	case len(vendor) == 0 && len(manual) == 0:
		v.Decision = DecisionGoWithPatches
		v.Summary = fmt.Sprintf("%d object(s) use APIs that disappear at the target version, and all of them are mechanically fixable. Apply the generated patch, re-run the scan, then upgrade.", len(auto))
	default:
		v.Decision = DecisionNoGo
		v.Summary = fmt.Sprintf("%d blocker(s) need a decision before the control plane moves: %d owned by a vendor chart or operator, %d needing a hand-written change. %d further object(s) are mechanically fixable.",
			len(vendor)+len(manual), len(vendor), len(manual), len(auto))
	}
	return v
}

func ownerLabel(o *scan.Owner) string {
	if o.Name == "" {
		return o.Kind
	}
	return o.Kind + " " + o.Name
}
