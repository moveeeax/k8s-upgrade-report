package scan

import (
	"fmt"
	"strings"
)

// Owner records that a resource is managed by something other than the team
// running the upgrade. Patching such a resource in git is pointless: the next
// reconcile overwrites it, and in the CRD case the controller that serves the
// custom resources has to ship the new API first.
//
// This is the noise-suppression step. Pluto and kubent report a vendor CRD the
// same way they report your own Ingress, which is why their output is ignored.
type Owner struct {
	Kind   string `json:"kind"`   // helm, argocd, flux, olm, operator, chart-vendored
	Name   string `json:"name"`   // release / application / operator name, when known
	Reason string `json:"reason"` // what was matched, so the finding is auditable
}

// Action is the plain-English instruction for a vendor-owned resource.
func (o Owner) Action() string {
	switch o.Kind {
	case "helm", "chart-vendored":
		return fmt.Sprintf("Upgrade the chart behind %s to a version that emits the new API, then re-render.", o.display())
	case "argocd", "flux":
		return fmt.Sprintf("Change the source of %s - editing the live object will be reverted on the next sync.", o.display())
	case "olm", "operator":
		return fmt.Sprintf("Upgrade the operator behind %s. It owns this object's schema.", o.display())
	case "crd":
		return "Upgrade the controller that ships this CustomResourceDefinition; do not hand-edit the CRD."
	default:
		return "Owned outside this repository - change it at the source."
	}
}

func (o Owner) display() string {
	if o.Name == "" {
		return "this resource"
	}
	return o.Name
}

// OwnerOf infers external ownership from the well-known labels, annotations
// and layout conventions. It deliberately errs towards "not owned": a false
// vendor label hides a real blocker.
func OwnerOf(r Resource) *Owner {
	if v := r.Annotations["meta.helm.sh/release-name"]; v != "" {
		return &Owner{Kind: "helm", Name: v, Reason: "meta.helm.sh/release-name annotation"}
	}
	if v := r.Annotations["argocd.argoproj.io/instance"]; v != "" {
		return &Owner{Kind: "argocd", Name: v, Reason: "argocd.argoproj.io/instance annotation"}
	}
	if v := r.Labels["kustomize.toolkit.fluxcd.io/name"]; v != "" {
		return &Owner{Kind: "flux", Name: v, Reason: "kustomize.toolkit.fluxcd.io/name label"}
	}
	if v := r.Annotations["olm.owner"]; v != "" {
		return &Owner{Kind: "olm", Name: v, Reason: "olm.owner annotation"}
	}
	switch strings.ToLower(r.Labels["app.kubernetes.io/managed-by"]) {
	case "helm", "tiller":
		name := firstNonEmpty(r.Labels["helm.sh/chart"], r.Labels["app.kubernetes.io/name"])
		return &Owner{Kind: "helm", Name: name, Reason: "app.kubernetes.io/managed-by=Helm label"}
	case "argocd", "argo-cd":
		return &Owner{Kind: "argocd", Name: r.Labels["app.kubernetes.io/instance"], Reason: "app.kubernetes.io/managed-by=argocd label"}
	}
	if seg, ok := vendorPathSegment(r.File); ok {
		return &Owner{Kind: "chart-vendored", Name: chartName(r.File), Reason: "lives under " + seg + "/ in the repository"}
	}
	if r.Kind == "CustomResourceDefinition" {
		return &Owner{Kind: "crd", Name: r.Name, Reason: "CustomResourceDefinition, served by the controller that ships it"}
	}
	return nil
}

func vendorPathSegment(path string) (string, bool) {
	for _, seg := range strings.Split(path, "/") {
		switch seg {
		case "charts", "vendor", "third_party", "upstream":
			return seg, true
		}
	}
	return "", false
}

func chartName(path string) string {
	parts := strings.Split(path, "/")
	for i, seg := range parts {
		if seg == "charts" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
