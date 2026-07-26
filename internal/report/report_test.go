package report_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/moveeeax/k8s-upgrade-report/internal/plan"
	"github.com/moveeeax/k8s-upgrade-report/internal/report"
	"github.com/moveeeax/k8s-upgrade-report/internal/rules"
	"github.com/moveeeax/k8s-upgrade-report/internal/scan"
)

func buildReport(t *testing.T) report.Report {
	t.Helper()
	set, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	files, skipped, err := scan.LoadDir("../../testdata/broken-cluster")
	if err != nil {
		t.Fatal(err)
	}
	current, target := rules.Minor{Major: 1, Minor: 24}, rules.Minor{Major: 1, Minor: 29}
	res := scan.Run(set, files, skipped, scan.Options{Current: current, Target: target})
	p := plan.Build(set, res, plan.Options{Current: current, Target: target, Provider: "eks"})
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	return report.Build("v0.1.0", "testdata/broken-cluster", at, res, p, 7)
}

func TestHTMLReportIsSelfContained(t *testing.T) {
	var buf bytes.Buffer
	if err := buildReport(t).WriteHTML(&buf); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		"NO-GO",
		"Vendor must upgrade first",
		"Upgrade sequence",
		"Amazon EKS",
		"vpc-cni",
		"app/ingress.yaml",
		"cert-manager",
		"Out of scope",
		"1.24 &rarr; 1.29",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML report is missing %q", want)
		}
	}
	// Self-contained means no network at render time: no scripts, no remote CSS.
	for _, forbidden := range []string{"<script", "src=\"http", "@import", "<link "} {
		if strings.Contains(html, forbidden) {
			t.Errorf("HTML report pulls in %q; it has to survive being an email attachment", forbidden)
		}
	}
	if !strings.HasPrefix(html, "<!DOCTYPE html>") {
		t.Error("HTML report should start with a doctype")
	}
}

func TestJSONReportShape(t *testing.T) {
	var buf bytes.Buffer
	if err := buildReport(t).WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got struct {
		Tool    string `json:"tool"`
		Summary struct {
			Blockers    int `json:"blockers"`
			VendorOwned int `json:"vendorOwned"`
			Autofixable int `json:"autofixable"`
		} `json:"summary"`
		Plan struct {
			Verdict struct {
				Decision string `json:"decision"`
			} `json:"verdict"`
			Hops []struct {
				To string `json:"to"`
			} `json:"hops"`
		} `json:"plan"`
		Findings []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Resource struct {
				File string `json:"file"`
				Line int    `json:"line"`
			} `json:"resource"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("report JSON does not parse: %v", err)
	}
	if got.Tool != "k8s-upgrade-report" {
		t.Errorf("tool = %q", got.Tool)
	}
	if got.Summary.Blockers == 0 || got.Summary.VendorOwned != 3 || got.Summary.Autofixable != 7 {
		t.Errorf("summary = %+v", got.Summary)
	}
	if got.Plan.Verdict.Decision != "no-go" || len(got.Plan.Hops) != 5 {
		t.Errorf("plan = %+v", got.Plan)
	}
	if len(got.Findings) == 0 {
		t.Fatal("no findings serialised")
	}
	for _, f := range got.Findings {
		if f.ID == "" || f.Severity == "" || f.Resource.File == "" || f.Resource.Line == 0 {
			t.Errorf("finding is missing fields a client report needs: %+v", f)
		}
	}
}

func TestTextReportLeadsWithTheVerdict(t *testing.T) {
	var buf bytes.Buffer
	if err := buildReport(t).WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "VERDICT: NO-GO") {
		t.Errorf("text report should lead with the verdict:\n%s", out)
	}
	for _, want := range []string{"BLOCKERS", "VENDOR MUST UPGRADE FIRST", "UPGRADE SEQUENCE", "addon floors:"} {
		if !strings.Contains(out, want) {
			t.Errorf("text report is missing the %q section", want)
		}
	}
}
