package scan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/moveeeax/k8s-upgrade-report/internal/rules"
)

// Severity ranks a finding for the go/no-go call.
type Severity string

const (
	// SeverityBlocker means the upgrade to the target version breaks this object.
	SeverityBlocker Severity = "blocker"
	// SeverityWarning means it breaks within the horizon after the target.
	SeverityWarning Severity = "warning"
	// SeverityInfo is worth writing down, not worth stopping for.
	SeverityInfo Severity = "info"
)

func severityRank(s Severity) int {
	switch s {
	case SeverityBlocker:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}

// Category groups findings in the report.
type Category string

const (
	// CategoryAPIRemoval is an object served by an API version that goes away.
	CategoryAPIRemoval Category = "api-removal"
	// CategoryUpgradeRisk is something that stalls or breaks the roll itself.
	CategoryUpgradeRisk Category = "upgrade-risk"
)

// ResourceRef points back at the manifest a finding came from.
type ResourceRef struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

// Finding is one thing to fix before or during the upgrade.
type Finding struct {
	ID          string      `json:"id"`
	RuleID      string      `json:"ruleId,omitempty"`
	Severity    Severity    `json:"severity"`
	Category    Category    `json:"category"`
	Title       string      `json:"title"`
	Message     string      `json:"message"`
	Remediation string      `json:"remediation"`
	Resource    ResourceRef `json:"resource"`
	Replacement string      `json:"replacement,omitempty"`
	RemovedIn   string      `json:"removedIn,omitempty"`
	Autofix     bool        `json:"autofix"`
	Owner       *Owner      `json:"owner,omitempty"`
	Docs        string      `json:"docs,omitempty"`

	resource Resource // kept for patch generation, not serialised
}

// Target returns the underlying resource, for the patch generator.
func (f Finding) Target() Resource { return f.resource }

// Options configures a scan.
type Options struct {
	Current rules.Minor // the version the cluster is on today
	Target  rules.Minor // the version being upgraded to
	Horizon int         // how many minors past the target to warn about; 0 means 3
}

// Result is everything one scan produced.
type Result struct {
	Findings  []Finding `json:"findings"`
	Skipped   []string  `json:"skipped,omitempty"`
	Files     int       `json:"filesScanned"`
	Resources int       `json:"resourcesScanned"`
}

// Blockers returns the findings that stop the upgrade at the target version.
func (r Result) Blockers() []Finding { return r.bySeverity(SeverityBlocker) }

// Warnings returns the findings that bite after the target version.
func (r Result) Warnings() []Finding { return r.bySeverity(SeverityWarning) }

func (r Result) bySeverity(s Severity) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == s {
			out = append(out, f)
		}
	}
	return out
}

// VendorBlocked returns blockers that belong to somebody else's chart or operator.
func (r Result) VendorBlocked() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Owner != nil && f.Severity == SeverityBlocker {
			out = append(out, f)
		}
	}
	return out
}

// Autofixable returns the blockers and warnings a generated patch can fix.
func (r Result) Autofixable() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Autofix {
			out = append(out, f)
		}
	}
	return out
}

// Run scans already-loaded files against the dataset.
func Run(set *rules.Set, files []File, skipped []string, opts Options) Result {
	if opts.Horizon == 0 {
		opts.Horizon = 3
	}
	warnUntil := opts.Target
	for i := 0; i < opts.Horizon; i++ {
		warnUntil = warnUntil.Next()
	}

	res := Result{Skipped: skipped, Files: len(files)}
	for _, f := range files {
		for _, r := range f.Resources {
			res.Resources++
			if finding, ok := apiRemovalFinding(set, r, opts, warnUntil); ok {
				res.Findings = append(res.Findings, finding)
			}
			res.Findings = append(res.Findings, upgradeRiskFindings(r, opts)...)
		}
	}
	sort.SliceStable(res.Findings, func(i, j int) bool {
		a, b := res.Findings[i], res.Findings[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra < rb
		}
		if a.Resource.File != b.Resource.File {
			return a.Resource.File < b.Resource.File
		}
		return a.Resource.Line < b.Resource.Line
	})
	return res
}

func apiRemovalFinding(set *rules.Set, r Resource, opts Options, warnUntil rules.Minor) (Finding, bool) {
	rule, ok := set.Lookup(r.APIVersion, r.Kind)
	if !ok {
		return Finding{}, false
	}
	removed, err := rules.ParseMinor(rule.RemovedIn)
	if err != nil {
		return Finding{}, false
	}

	var sev Severity
	switch {
	case rules.MinorLessOrEqual(removed, opts.Target):
		sev = SeverityBlocker
	case rules.MinorLessOrEqual(removed, warnUntil):
		sev = SeverityWarning
	default:
		sev = SeverityInfo
	}

	owner := OwnerOf(r)
	f := Finding{
		ID:          rule.ID,
		RuleID:      rule.ID,
		Severity:    sev,
		Category:    CategoryAPIRemoval,
		Title:       fmt.Sprintf("%s %s is removed in %s", r.Kind, r.APIVersion, rule.RemovedIn),
		Replacement: rule.To,
		RemovedIn:   rule.RemovedIn,
		Owner:       owner,
		Docs:        rule.Docs,
		Autofix:     rule.Autofixable() && owner == nil,
		Resource: ResourceRef{
			File: r.File, Line: r.StartLine, APIVersion: r.APIVersion,
			Kind: r.Kind, Name: r.Name, Namespace: r.Namespace,
		},
		resource: r,
	}

	msg := &strings.Builder{}
	fmt.Fprintf(msg, "%s was deprecated in %s and is no longer served from %s.", r.APIVersion, rule.DeprecatedIn, rule.RemovedIn)
	if opts.Current != (rules.Minor{}) && rules.MinorLessOrEqual(removed, opts.Current) {
		fmt.Fprintf(msg, " The cluster is already on %s, so this object is not being served today.", opts.Current)
	}
	if rule.Notes != "" {
		fmt.Fprintf(msg, " %s", strings.TrimSpace(rule.Notes))
	}
	f.Message = msg.String()

	switch {
	case owner != nil:
		f.Remediation = owner.Action()
	case rule.To == "":
		f.Remediation = "No replacement API exists - this needs a design decision, not a rewrite."
	case rule.Autofixable():
		f.Remediation = fmt.Sprintf("Rewrite to %s. `k8s-upgrade-report patch` generates the diff.", rule.To)
	default:
		f.Remediation = fmt.Sprintf("Rewrite to %s by hand - the field changes are not mechanical.", rule.To)
	}
	return f, true
}
