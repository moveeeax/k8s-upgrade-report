package plan_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moveeeax/k8s-upgrade-report/internal/plan"
	"github.com/moveeeax/k8s-upgrade-report/internal/rules"
	"github.com/moveeeax/k8s-upgrade-report/internal/scan"
)

func build(t *testing.T, dir string, current, target rules.Minor, provider string) plan.Plan {
	t.Helper()
	set, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	files, skipped, err := scan.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := scan.Run(set, files, skipped, scan.Options{Current: current, Target: target})
	return plan.Build(set, res, plan.Options{Current: current, Target: target, Provider: provider})
}

func TestSequencingHopsAndAddonFloors(t *testing.T) {
	p := build(t, "../../testdata/broken-cluster", rules.Minor{Major: 1, Minor: 24}, rules.Minor{Major: 1, Minor: 29}, "eks")

	if len(p.Hops) != 5 {
		t.Fatalf("1.24 -> 1.29 should be 5 hops, got %d", len(p.Hops))
	}
	if p.Hops[0].From != "1.24" || p.Hops[0].To != "1.25" {
		t.Errorf("first hop = %s -> %s, want 1.24 -> 1.25", p.Hops[0].From, p.Hops[0].To)
	}
	if p.ProviderName != "Amazon EKS" || len(p.Sequencing) == 0 {
		t.Errorf("provider sequencing missing: %q %v", p.ProviderName, p.Sequencing)
	}

	// The 1.25 hop is where PSP, CronJob, PDB and the v2beta1 HPA all land.
	got := map[string]int{}
	for _, r := range p.Hops[0].Removals {
		got[r.Rule.ID] = r.Hits
	}
	for _, id := range []string{"podsecuritypolicy-v1beta1", "cronjob-batch-v1beta1", "hpa-autoscaling-v2beta1"} {
		if got[id] == 0 {
			t.Errorf("hop 1.24 -> 1.25 should list %s, got %v", id, got)
		}
	}
	if got["poddisruptionbudget-policy-v1beta1"] != 2 {
		t.Errorf("expected 2 PodDisruptionBudget hits at 1.25, got %d", got["poddisruptionbudget-policy-v1beta1"])
	}
	if floor := p.Hops[0].AddonFloors["vpc-cni"]; floor == "" {
		t.Error("EKS addon floors are missing for the 1.25 hop")
	}

	// A removal that nothing in the repository uses does not clutter the plan.
	for _, h := range p.Hops {
		for _, r := range h.Removals {
			if r.Hits == 0 {
				t.Errorf("hop %s lists %s with zero hits", h.To, r.Rule.ID)
			}
		}
	}
}

func TestVerdictNoGoNamesTheBlockers(t *testing.T) {
	p := build(t, "../../testdata/broken-cluster", rules.Minor{Major: 1, Minor: 24}, rules.Minor{Major: 1, Minor: 29}, "eks")
	if p.Verdict.Decision != plan.DecisionNoGo {
		t.Fatalf("decision = %s, want no-go", p.Verdict.Decision)
	}
	joined := strings.Join(p.Verdict.Blockers, "\n")
	for _, want := range []string{"cert-manager", "ingress-nginx", "PodSecurityPolicy"} {
		if !strings.Contains(joined, want) {
			t.Errorf("verdict should name %s as a blocker:\n%s", want, joined)
		}
	}
	if len(p.Verdict.OutOfScope) == 0 {
		t.Error("the report must state what is out of scope")
	}
}

func TestVerdictGoAfterPatchesWhenEverythingIsMechanical(t *testing.T) {
	dir := t.TempDir()
	manifest := `apiVersion: batch/v1beta1
kind: CronJob
metadata:
  name: nightly
spec:
  schedule: "0 1 * * *"
`
	if err := os.WriteFile(filepath.Join(dir, "cronjob.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	p := build(t, dir, rules.Minor{Major: 1, Minor: 24}, rules.Minor{Major: 1, Minor: 25}, "gke")
	if p.Verdict.Decision != plan.DecisionGoWithPatches {
		t.Fatalf("decision = %s, want %s", p.Verdict.Decision, plan.DecisionGoWithPatches)
	}
	if len(p.Verdict.Blockers) != 0 {
		t.Errorf("nothing needs a human decision here, got %v", p.Verdict.Blockers)
	}
}

func TestVerdictGoOnCleanManifests(t *testing.T) {
	p := build(t, "../../testdata/clean", rules.Minor{Major: 1, Minor: 28}, rules.Minor{Major: 1, Minor: 30}, "aks")
	if p.Verdict.Decision != plan.DecisionGo {
		t.Fatalf("decision = %s, want go (%v)", p.Verdict.Decision, p.Verdict.Blockers)
	}
	if p.ProviderName != "Azure AKS" {
		t.Errorf("provider name = %q", p.ProviderName)
	}
}
