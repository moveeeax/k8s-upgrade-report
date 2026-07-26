package rules

import "testing"

func TestLoadDataset(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Rules()) < 20 {
		t.Fatalf("expected the dataset to cover at least 20 removals, got %d", len(set.Rules()))
	}

	for _, r := range set.Rules() {
		dep, err := ParseMinor(r.DeprecatedIn)
		if err != nil {
			t.Fatalf("rule %s: deprecatedIn: %v", r.ID, err)
		}
		rem, err := ParseMinor(r.RemovedIn)
		if err != nil {
			t.Fatalf("rule %s: removedIn: %v", r.ID, err)
		}
		if !MinorLess(dep, rem) {
			t.Errorf("rule %s: deprecatedIn %s is not before removedIn %s", r.ID, dep, rem)
		}
		if r.Docs == "" {
			t.Errorf("rule %s: every rule needs a docs link, it ends up in a client report", r.ID)
		}
		if r.Autofixable() && r.To == "" {
			t.Errorf("rule %s: claims to be autofixable but has no replacement API", r.ID)
		}
		if r.Transform == TransformManual && r.Autofixable() {
			t.Errorf("rule %s: manual transform must not be autofixable", r.ID)
		}
	}
}

func TestLookupKnownRemovals(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		apiVersion, kind, removedIn, to string
		transform                       Transform
	}{
		{"extensions/v1beta1", "Ingress", "1.22", "networking.k8s.io/v1", TransformIngressV1},
		{"policy/v1beta1", "PodSecurityPolicy", "1.25", "", TransformManual},
		{"batch/v1beta1", "CronJob", "1.25", "batch/v1", TransformAPIVersion},
		{"autoscaling/v2beta1", "HorizontalPodAutoscaler", "1.25", "autoscaling/v2", TransformHPAV2},
		{"flowcontrol.apiserver.k8s.io/v1beta2", "FlowSchema", "1.29", "flowcontrol.apiserver.k8s.io/v1", TransformAPIVersion},
	}
	for _, c := range cases {
		r, ok := set.Lookup(c.apiVersion, c.kind)
		if !ok {
			t.Fatalf("%s %s is not in the dataset", c.apiVersion, c.kind)
		}
		if r.RemovedIn != c.removedIn || r.To != c.to || r.Transform != c.transform {
			t.Errorf("%s %s: got removedIn=%s to=%q transform=%s, want removedIn=%s to=%q transform=%s",
				c.apiVersion, c.kind, r.RemovedIn, r.To, r.Transform, c.removedIn, c.to, c.transform)
		}
	}
	if _, ok := set.Lookup("apps/v1", "Deployment"); ok {
		t.Error("apps/v1 Deployment must not be reported as removed")
	}
}

func TestProviders(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"eks", "aks", "gke"} {
		p, ok := set.Provider(name)
		if !ok {
			t.Fatalf("provider %s missing", name)
		}
		if len(p.Sequencing) == 0 {
			t.Errorf("provider %s has no sequencing steps", name)
		}
		if len(p.Addons) == 0 {
			t.Errorf("provider %s has no addon floors", name)
		}
	}
	if _, ok := set.Provider("openshift"); ok {
		t.Error("openshift is explicitly out of scope and must not resolve")
	}
}

func TestParseMinor(t *testing.T) {
	cases := map[string]Minor{
		"1.29":             {1, 29},
		"v1.29":            {1, 29},
		"1.29.4":           {1, 29},
		"v1.27.9-eks-1234": {1, 27},
	}
	for in, want := range cases {
		got, err := ParseMinor(in)
		if err != nil {
			t.Fatalf("ParseMinor(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseMinor(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "1", "next", "1.x"} {
		if _, err := ParseMinor(bad); err == nil {
			t.Errorf("ParseMinor(%q) should have failed", bad)
		}
	}
}

func TestPath(t *testing.T) {
	got := Path(Minor{1, 24}, Minor{1, 27})
	want := []Minor{{1, 25}, {1, 26}, {1, 27}}
	if len(got) != len(want) {
		t.Fatalf("Path returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Path returned %v, want %v", got, want)
		}
	}
	if n := len(Path(Minor{1, 29}, Minor{1, 29})); n != 0 {
		t.Errorf("Path with no hops returned %d entries", n)
	}
}
