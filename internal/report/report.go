// Package report renders a scan plus a plan as JSON or as one self-contained
// HTML file that can be mailed to a client or published as a static page.
package report

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/moveeeax/k8s-upgrade-report/internal/plan"
	"github.com/moveeeax/k8s-upgrade-report/internal/scan"
)

//go:embed report.html.tmpl
var templateFS embed.FS

// Summary is the count block at the top of the report.
type Summary struct {
	Blockers    int `json:"blockers"`
	Warnings    int `json:"warnings"`
	VendorOwned int `json:"vendorOwned"`
	Autofixable int `json:"autofixable"`
	Resources   int `json:"resourcesScanned"`
	Files       int `json:"filesScanned"`
}

// Report is the serialisable form of one run.
type Report struct {
	Tool         string         `json:"tool"`
	Version      string         `json:"version"`
	GeneratedAt  string         `json:"generatedAt"`
	Source       string         `json:"source"`
	Summary      Summary        `json:"summary"`
	Plan         plan.Plan      `json:"plan"`
	Findings     []scan.Finding `json:"findings"`
	Skipped      []string       `json:"skipped,omitempty"`
	PatchedCount int            `json:"patchedCount"`
}

// Build assembles a report from a scan result and a plan.
func Build(version, source string, at time.Time, res scan.Result, p plan.Plan, patched int) Report {
	return Report{
		Tool:        "k8s-upgrade-report",
		Version:     version,
		GeneratedAt: at.UTC().Format(time.RFC3339),
		Source:      source,
		Summary: Summary{
			Blockers:    len(res.Blockers()),
			Warnings:    len(res.Warnings()),
			VendorOwned: len(res.VendorBlocked()),
			Autofixable: len(res.Autofixable()),
			Resources:   res.Resources,
			Files:       res.Files,
		},
		Plan:         p,
		Findings:     res.Findings,
		Skipped:      res.Skipped,
		PatchedCount: patched,
	}
}

// WriteJSON writes the machine-readable form.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

type htmlView struct {
	Report
	Blockers   []scan.Finding
	Vendor     []scan.Finding
	Warnings   []scan.Finding
	Info       []scan.Finding
	VerdictCSS string
}

// WriteHTML writes the single-file report. No external CSS, no JavaScript, no
// fonts: it has to survive being attached to an email.
func (r Report) WriteHTML(w io.Writer) error {
	tmpl, err := template.New("report.html.tmpl").Funcs(template.FuncMap{
		"upper": func(v any) string { return strings.ToUpper(fmt.Sprint(v)) },
	}).ParseFS(templateFS, "report.html.tmpl")
	if err != nil {
		return err
	}

	v := htmlView{Report: r}
	for _, f := range r.Findings {
		switch {
		case f.Severity == scan.SeverityBlocker && f.Owner != nil:
			v.Vendor = append(v.Vendor, f)
		case f.Severity == scan.SeverityBlocker:
			v.Blockers = append(v.Blockers, f)
		case f.Severity == scan.SeverityWarning:
			v.Warnings = append(v.Warnings, f)
		default:
			v.Info = append(v.Info, f)
		}
	}
	switch r.Plan.Verdict.Decision {
	case plan.DecisionGo:
		v.VerdictCSS = "go"
	case plan.DecisionGoWithPatches:
		v.VerdictCSS = "patch"
	default:
		v.VerdictCSS = "nogo"
	}
	return tmpl.Execute(w, v)
}

// WriteText writes the terminal summary.
func (r Report) WriteText(w io.Writer) error {
	fmt.Fprintf(w, "k8s-upgrade-report %s  %s -> %s", r.Version, r.Plan.Current, r.Plan.Target)
	if r.Plan.ProviderName != "" {
		fmt.Fprintf(w, "  (%s)", r.Plan.ProviderName)
	}
	fmt.Fprintf(w, "\nsource: %s\n\n", r.Source)
	fmt.Fprintf(w, "VERDICT: %s\n%s\n\n", strings.ToUpper(string(r.Plan.Verdict.Decision)), wrap(r.Plan.Verdict.Summary, 78))

	fmt.Fprintf(w, "%d file(s), %d object(s) scanned: %d blocker(s), %d warning(s), %d vendor-owned, %d auto-fixable\n\n",
		r.Summary.Files, r.Summary.Resources, r.Summary.Blockers, r.Summary.Warnings, r.Summary.VendorOwned, r.Summary.Autofixable)

	printSection(w, "BLOCKERS", filter(r.Findings, func(f scan.Finding) bool {
		return f.Severity == scan.SeverityBlocker && f.Owner == nil
	}))
	printSection(w, "VENDOR MUST UPGRADE FIRST", filter(r.Findings, func(f scan.Finding) bool {
		return f.Severity == scan.SeverityBlocker && f.Owner != nil
	}))
	printSection(w, "WARNINGS (bite after the target version)", filter(r.Findings, func(f scan.Finding) bool {
		return f.Severity == scan.SeverityWarning
	}))

	if len(r.Plan.Hops) > 0 {
		fmt.Fprintf(w, "UPGRADE SEQUENCE\n")
		for _, h := range r.Plan.Hops {
			fmt.Fprintf(w, "  %s -> %s: %d affected object(s)\n", h.From, h.To, h.Hits)
			for _, rm := range h.Removals {
				fmt.Fprintf(w, "      %-46s %d object(s)\n", rm.Rule.From+" "+rm.Rule.Kind, rm.Hits)
			}
			if len(h.AddonFloors) > 0 {
				fmt.Fprintf(w, "      addon floors: %s\n", joinMap(h.AddonFloors))
			}
		}
		fmt.Fprintln(w)
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintf(w, "SKIPPED (%d)\n", len(r.Skipped))
		for _, s := range r.Skipped {
			fmt.Fprintf(w, "  %s\n", s)
		}
		fmt.Fprintln(w)
	}
	return nil
}

func printSection(w io.Writer, title string, fs []scan.Finding) {
	if len(fs) == 0 {
		return
	}
	fmt.Fprintf(w, "%s (%d)\n", title, len(fs))
	for _, f := range fs {
		fmt.Fprintf(w, "  %s:%d  %s %s\n", f.Resource.File, f.Resource.Line, f.Resource.Kind, f.Resource.Name)
		fmt.Fprintf(w, "      %s\n", f.Title)
		if f.Owner != nil {
			fmt.Fprintf(w, "      owner: %s (%s)\n", ownerText(f.Owner), f.Owner.Reason)
		}
		fmt.Fprintf(w, "      fix: %s\n", wrapIndent(f.Remediation, 74, "           "))
	}
	fmt.Fprintln(w)
}

func ownerText(o *scan.Owner) string {
	if o.Name == "" {
		return o.Kind
	}
	return o.Kind + " " + o.Name
}

func filter(in []scan.Finding, keep func(scan.Finding) bool) []scan.Finding {
	var out []scan.Finding
	for _, f := range in {
		if keep(f) {
			out = append(out, f)
		}
	}
	return out
}

func joinMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" >= "+m[k])
	}
	return strings.Join(parts, ", ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func wrap(s string, width int) string { return wrapIndent(s, width, "") }

func wrapIndent(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	lineLen := 0
	for i, w := range words {
		if lineLen > 0 && lineLen+1+len(w) > width {
			b.WriteString("\n" + indent)
			lineLen = 0
		} else if i > 0 {
			b.WriteString(" ")
			lineLen++
		}
		b.WriteString(w)
		lineLen += len(w)
	}
	return b.String()
}
