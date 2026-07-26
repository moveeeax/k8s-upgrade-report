package scan_test

import (
	"strings"
	"testing"

	"github.com/moveeeax/k8s-upgrade-report/internal/rules"
	"github.com/moveeeax/k8s-upgrade-report/internal/scan"
)

const brokenFixture = "../../testdata/broken-cluster"

func runFixture(t *testing.T, dir string, current, target rules.Minor) (scan.Result, []scan.File) {
	t.Helper()
	set, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	files, skipped, err := scan.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir(%s): %v", dir, err)
	}
	return scan.Run(set, files, skipped, scan.Options{Current: current, Target: target}), files
}

func find(t *testing.T, res scan.Result, file, id string) scan.Finding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Resource.File == file && f.ID == id {
			return f
		}
	}
	t.Fatalf("no finding %q in %s; got %v", id, file, ids(res))
	return scan.Finding{}
}

func ids(res scan.Result) []string {
	var out []string
	for _, f := range res.Findings {
		out = append(out, f.Resource.File+":"+f.ID)
	}
	return out
}

func TestScanBrokenClusterFindsEveryRemoval(t *testing.T) {
	res, _ := runFixture(t, brokenFixture, rules.Minor{Major: 1, Minor: 24}, rules.Minor{Major: 1, Minor: 29})

	want := map[string]string{
		"app/ingress.yaml":                    "ingress-extensions-v1beta1",
		"app/workloads.yaml":                  "cronjob-batch-v1beta1",
		"platform/psp.yaml":                   "podsecuritypolicy-v1beta1",
		"platform/apiserver-flowcontrol.yaml": "flowschema-v1beta2",
		"charts/ingress-nginx/rendered.yaml":  "ingress-networking-v1beta1",
		"platform/cert-manager-crd.yaml":      "crd-apiextensions-v1beta1",
		"platform/monitoring-hpa.yaml":        "hpa-autoscaling-v2beta2",
	}
	for file, id := range want {
		f := find(t, res, file, id)
		if f.Severity != scan.SeverityBlocker {
			t.Errorf("%s: %s should be a blocker for a 1.29 target, got %s", file, id, f.Severity)
		}
	}
}

func TestVendorOwnedResourcesAreNeverAutofixed(t *testing.T) {
	res, _ := runFixture(t, brokenFixture, rules.Minor{Major: 1, Minor: 24}, rules.Minor{Major: 1, Minor: 29})

	cases := []struct {
		file, id, ownerKind, ownerName string
	}{
		{"charts/ingress-nginx/rendered.yaml", "ingress-networking-v1beta1", "helm", "ingress-nginx"},
		{"platform/cert-manager-crd.yaml", "crd-apiextensions-v1beta1", "helm", "cert-manager"},
		{"platform/monitoring-hpa.yaml", "hpa-autoscaling-v2beta2", "argocd", "monitoring"},
	}
	for _, c := range cases {
		f := find(t, res, c.file, c.id)
		if f.Owner == nil {
			t.Fatalf("%s: expected vendor ownership to be detected", c.file)
		}
		if f.Owner.Kind != c.ownerKind || f.Owner.Name != c.ownerName {
			t.Errorf("%s: owner = %s/%s, want %s/%s", c.file, f.Owner.Kind, f.Owner.Name, c.ownerKind, c.ownerName)
		}
		if f.Autofix {
			t.Errorf("%s: vendor-owned resources must never be marked autofixable", c.file)
		}
		if !strings.Contains(strings.ToLower(f.Remediation), "upgrade") && !strings.Contains(strings.ToLower(f.Remediation), "source") {
			t.Errorf("%s: remediation should point at the vendor, got %q", c.file, f.Remediation)
		}
	}

	// Objects the team owns are still autofixable.
	own := find(t, res, "app/workloads.yaml", "cronjob-batch-v1beta1")
	if !own.Autofix {
		t.Error("a plain CronJob in the team's own directory should be autofixable")
	}
	if own.Owner != nil {
		t.Errorf("unexpected owner on own CronJob: %+v", own.Owner)
	}
}

func TestPodSecurityPolicyHasNoAutomaticFix(t *testing.T) {
	res, _ := runFixture(t, brokenFixture, rules.Minor{Major: 1, Minor: 24}, rules.Minor{Major: 1, Minor: 25})
	f := find(t, res, "platform/psp.yaml", "podsecuritypolicy-v1beta1")
	if f.Autofix {
		t.Error("PodSecurityPolicy has no replacement API and must not be autofixable")
	}
	if f.Replacement != "" {
		t.Errorf("PodSecurityPolicy replacement = %q, want empty", f.Replacement)
	}
}

func TestUpgradeRiskChecks(t *testing.T) {
	res, _ := runFixture(t, brokenFixture, rules.Minor{Major: 1, Minor: 24}, rules.Minor{Major: 1, Minor: 29})

	drain := find(t, res, "app/workloads.yaml", "pdb-blocks-drain")
	if drain.Severity != scan.SeverityBlocker {
		t.Errorf("a PDB with maxUnavailable: 0 must be a blocker, got %s", drain.Severity)
	}
	if drain.Resource.Name != "storefront" {
		t.Errorf("pdb-blocks-drain hit the wrong object: %s", drain.Resource.Name)
	}

	sel := find(t, res, "app/workloads.yaml", "pdb-empty-selector")
	if sel.Resource.Name != "catch-all" {
		t.Errorf("pdb-empty-selector hit the wrong object: %s", sel.Resource.Name)
	}

	label := find(t, res, "app/workloads.yaml", "retired-beta-node-labels")
	if label.Resource.Kind != "Deployment" {
		t.Errorf("retired-beta-node-labels hit %s, want Deployment", label.Resource.Kind)
	}
}

func TestSeverityFollowsTheTargetVersion(t *testing.T) {
	// FlowSchema v1beta2 goes away in 1.29: a blocker at 1.29, a warning at 1.26.
	at29, _ := runFixture(t, brokenFixture, rules.Minor{Major: 1, Minor: 28}, rules.Minor{Major: 1, Minor: 29})
	if got := find(t, at29, "platform/apiserver-flowcontrol.yaml", "flowschema-v1beta2").Severity; got != scan.SeverityBlocker {
		t.Errorf("severity at target 1.29 = %s, want blocker", got)
	}
	at26, _ := runFixture(t, brokenFixture, rules.Minor{Major: 1, Minor: 25}, rules.Minor{Major: 1, Minor: 26})
	if got := find(t, at26, "platform/apiserver-flowcontrol.yaml", "flowschema-v1beta2").Severity; got != scan.SeverityWarning {
		t.Errorf("severity at target 1.26 = %s, want warning", got)
	}
}

func TestUnrenderedHelmTemplatesAreSkippedNotGuessed(t *testing.T) {
	res, _ := runFixture(t, brokenFixture, rules.Minor{Major: 1, Minor: 24}, rules.Minor{Major: 1, Minor: 29})
	var found bool
	for _, s := range res.Skipped {
		if strings.HasPrefix(s, "app/templates-hpa.yaml") {
			found = true
		}
	}
	if !found {
		t.Errorf("the unrendered Helm template should be reported as skipped, got %v", res.Skipped)
	}
	for _, f := range res.Findings {
		if f.Resource.File == "app/templates-hpa.yaml" {
			t.Errorf("no findings may be produced from an unrendered template: %+v", f)
		}
	}
}

func TestCleanManifestsProduceNoBlockers(t *testing.T) {
	res, _ := runFixture(t, "../../testdata/clean", rules.Minor{Major: 1, Minor: 28}, rules.Minor{Major: 1, Minor: 31})
	if n := len(res.Blockers()); n != 0 {
		t.Errorf("clean manifests produced %d blocker(s): %v", n, ids(res))
	}
	if res.Resources != 2 {
		t.Errorf("scanned %d objects, want 2", res.Resources)
	}
}
