package scan

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// upgradeRiskFindings covers the things that stall an upgrade even though no
// API is being removed: a PodDisruptionBudget that can never be satisfied, a
// PDB selector whose meaning flips on migration, and node labels that stopped
// being set on newly joined nodes. None of these are reported by kubent or
// Pluto, and every one of them has stalled a real node-group roll.
func upgradeRiskFindings(r Resource, opts Options) []Finding {
	var out []Finding
	if r.Kind == "PodDisruptionBudget" {
		out = append(out, pdbFindings(r)...)
	}
	if f, ok := betaNodeLabelFinding(r); ok {
		out = append(out, f)
	}
	_ = opts
	return out
}

func pdbFindings(r Resource) []Finding {
	spec := specOf(r)
	if spec == nil {
		return nil
	}
	var out []Finding
	maxUnavailable := ScalarAt(spec, "maxUnavailable")
	minAvailable := ScalarAt(spec, "minAvailable")

	if maxUnavailable == "0" || maxUnavailable == "0%" || minAvailable == "100%" {
		out = append(out, newRiskFinding(r, "pdb-blocks-drain", SeverityBlocker,
			"PodDisruptionBudget makes node drains impossible",
			"This budget allows zero voluntary disruption, so `kubectl drain` never completes and a managed node-group upgrade stalls until the cloud provider times out and rolls back.",
			"Relax the budget for the duration of the upgrade (maxUnavailable: 1) or delete it during the roll and restore it afterwards."))
	}

	if sel := MapAt(spec, "selector"); sel != nil && len(sel.Content) == 0 && r.APIVersion == "policy/v1beta1" {
		out = append(out, newRiskFinding(r, "pdb-empty-selector", SeverityWarning,
			"Empty PodDisruptionBudget selector changes meaning in policy/v1",
			"In policy/v1beta1 an empty selector selects no pods. In policy/v1 it selects every pod in the namespace, so this budget starts applying to workloads it never covered before.",
			"Write the selector out explicitly before migrating this object to policy/v1."))
	}
	return out
}

var retiredLabelPrefixes = []string{
	"beta.kubernetes.io/",
	"failure-domain.beta.kubernetes.io/",
}

func betaNodeLabelFinding(r Resource) (Finding, bool) {
	var hit string
	walkScalars(r.Doc, func(v string) {
		if hit != "" {
			return
		}
		for _, p := range retiredLabelPrefixes {
			if strings.HasPrefix(v, p) {
				hit = v
				return
			}
		}
	})
	if hit == "" {
		return Finding{}, false
	}
	return newRiskFinding(r, "retired-beta-node-labels", SeverityWarning,
		"Scheduling depends on a retired beta node label",
		fmt.Sprintf("This object references %q. Beta node labels stopped being applied to newly joined nodes, so pods pinned to them go unschedulable exactly when the upgrade replaces the node pool.", hit),
		"Switch to the GA equivalent (kubernetes.io/os, kubernetes.io/arch, topology.kubernetes.io/zone, node.kubernetes.io/instance-type) before rolling nodes."), true
}

func newRiskFinding(r Resource, id string, sev Severity, title, message, remediation string) Finding {
	return Finding{
		ID:          id,
		Severity:    sev,
		Category:    CategoryUpgradeRisk,
		Title:       title,
		Message:     message,
		Remediation: remediation,
		Owner:       OwnerOf(r),
		Resource: ResourceRef{
			File: r.File, Line: r.StartLine, APIVersion: r.APIVersion,
			Kind: r.Kind, Name: r.Name, Namespace: r.Namespace,
		},
		resource: r,
	}
}

func specOf(r Resource) *yaml.Node {
	if r.Doc == nil || len(r.Doc.Content) == 0 {
		return nil
	}
	return MapAt(r.Doc.Content[0], "spec")
}

func walkScalars(n *yaml.Node, fn func(string)) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		fn(n.Value)
		return
	}
	for _, c := range n.Content {
		walkScalars(c, fn)
	}
}
