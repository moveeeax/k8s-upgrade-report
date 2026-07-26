package patch_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moveeeax/k8s-upgrade-report/internal/patch"
	"github.com/moveeeax/k8s-upgrade-report/internal/rules"
	"github.com/moveeeax/k8s-upgrade-report/internal/scan"
	"gopkg.in/yaml.v3"
)

const fixture = "../../testdata/broken-cluster"

func generate(t *testing.T, dir string) patch.Result {
	t.Helper()
	set, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	files, skipped, err := scan.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := scan.Run(set, files, skipped, scan.Options{
		Current: rules.Minor{Major: 1, Minor: 24},
		Target:  rules.Minor{Major: 1, Minor: 29},
	})
	pr, err := patch.Generate(set, files, res.Findings)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return pr
}

func TestGeneratedPatchCoversOnlyOwnedResources(t *testing.T) {
	pr := generate(t, fixture)

	joined := strings.Join(pr.Patched, "\n")
	for _, want := range []string{
		"app/ingress.yaml", "app/workloads.yaml",
		"platform/apiserver-flowcontrol.yaml", "platform/psp.yaml",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %s to be patched, got:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{"charts/ingress-nginx", "cert-manager-crd", "monitoring-hpa"} {
		if strings.Contains(pr.Diff, unwanted) {
			t.Errorf("vendor-owned file %s must not appear in the patch", unwanted)
		}
	}
	if strings.Contains(pr.Diff, "kind: PodSecurityPolicy") {
		t.Error("PodSecurityPolicy has no replacement API and must not be rewritten")
	}
	if len(pr.Skipped) != 0 {
		t.Errorf("unexpected skipped rewrites: %v", pr.Skipped)
	}
}

// TestPatchAppliesAndFixesTheCluster is the end-to-end contract: the diff has
// to apply with git and the patched tree has to come back clean.
func TestPatchAppliesAndFixesTheCluster(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	copyTree(t, fixture, dir)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("init", "-q")
	run("add", "-A")

	pr := generate(t, dir)
	patchFile := filepath.Join(t.TempDir(), "upgrade.patch")
	if err := os.WriteFile(patchFile, []byte(pr.Diff), 0o600); err != nil {
		t.Fatal(err)
	}
	run("apply", "--check", patchFile)
	run("apply", patchFile)

	// Every mechanically fixable blocker is gone after applying the patch.
	set, err := rules.Load()
	if err != nil {
		t.Fatal(err)
	}
	files, skipped, err := scan.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := scan.Run(set, files, skipped, scan.Options{
		Current: rules.Minor{Major: 1, Minor: 24},
		Target:  rules.Minor{Major: 1, Minor: 29},
	})
	for _, f := range res.Findings {
		if f.Autofix {
			t.Errorf("still autofixable after applying the patch: %s %s in %s",
				f.Resource.Kind, f.Resource.Name, f.Resource.File)
		}
	}

	// And the rewritten Ingress really is a valid networking.k8s.io/v1 object.
	raw, err := os.ReadFile(filepath.Join(dir, "app", "ingress.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var ing struct {
		APIVersion string `yaml:"apiVersion"`
		Spec       struct {
			DefaultBackend struct {
				Service struct {
					Name string `yaml:"name"`
					Port struct {
						Number int    `yaml:"number"`
						Name   string `yaml:"name"`
					} `yaml:"port"`
				} `yaml:"service"`
			} `yaml:"defaultBackend"`
			Rules []struct {
				HTTP struct {
					Paths []struct {
						Path     string `yaml:"path"`
						PathType string `yaml:"pathType"`
						Backend  struct {
							Service struct {
								Name string `yaml:"name"`
								Port struct {
									Number int    `yaml:"number"`
									Name   string `yaml:"name"`
								} `yaml:"port"`
							} `yaml:"service"`
						} `yaml:"backend"`
					} `yaml:"paths"`
				} `yaml:"http"`
			} `yaml:"rules"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &ing); err != nil {
		t.Fatalf("patched ingress is not valid YAML: %v", err)
	}
	if ing.APIVersion != "networking.k8s.io/v1" {
		t.Errorf("apiVersion = %s, want networking.k8s.io/v1", ing.APIVersion)
	}
	if got := ing.Spec.DefaultBackend.Service.Name; got != "storefront" {
		t.Errorf("spec.backend was not moved to spec.defaultBackend (got name %q)", got)
	}
	if got := ing.Spec.DefaultBackend.Service.Port.Number; got != 80 {
		t.Errorf("defaultBackend port number = %d, want 80", got)
	}
	if len(ing.Spec.Rules) != 1 || len(ing.Spec.Rules[0].HTTP.Paths) != 2 {
		t.Fatalf("expected 1 rule with 2 paths, got %+v", ing.Spec.Rules)
	}
	p0 := ing.Spec.Rules[0].HTTP.Paths[0]
	if p0.PathType != "ImplementationSpecific" {
		t.Errorf("pathType = %q, want ImplementationSpecific", p0.PathType)
	}
	if p0.Backend.Service.Name != "storefront" || p0.Backend.Service.Port.Number != 80 {
		t.Errorf("numeric servicePort was not converted to port.number: %+v", p0.Backend.Service)
	}
	p1 := ing.Spec.Rules[0].HTTP.Paths[1]
	if p1.Backend.Service.Port.Name != "http" || p1.Backend.Service.Port.Number != 0 {
		t.Errorf("named servicePort was not converted to port.name: %+v", p1.Backend.Service.Port)
	}

	// The file comment survived the rewrite exactly once.
	if n := strings.Count(string(raw), "# The public ingress."); n != 1 {
		t.Errorf("head comment appears %d times after patching, want 1", n)
	}

	// The HPA metric was restructured, not just relabelled.
	wl, err := os.ReadFile(filepath.Join(dir, "app", "workloads.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"apiVersion: autoscaling/v2", "type: Utilization", "averageUtilization: 70"} {
		if !strings.Contains(string(wl), want) {
			t.Errorf("patched workloads.yaml is missing %q", want)
		}
	}
	if strings.Contains(string(wl), "targetAverageUtilization") {
		t.Error("targetAverageUtilization survived the autoscaling/v2 rewrite")
	}
	// Untouched documents in the same file kept their formatting.
	if !strings.Contains(string(wl), "image: registry.example.com/shop/reindex:1.4.2") {
		t.Error("an unrelated document in the same file was mangled")
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
