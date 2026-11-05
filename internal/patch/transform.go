// Package patch turns autofixable findings into a git-applyable unified diff.
// It never talks to a cluster and never writes to one: the output is a patch a
// human reads, reviews and applies.
package patch

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// transformDoc rewrites the mapping in place for the named transform.
// It returns an error when the object uses a shape the mechanical rewrite
// cannot handle, in which case the finding stays unpatched rather than being
// silently mangled.
func transformDoc(kind string, root *yaml.Node, to string) error {
	switch kind {
	case "ingress-v1":
		return transformIngress(root, to)
	case "hpa-v2":
		return transformHPA(root, to)
	default:
		return fmt.Errorf("unknown transform %q", kind)
	}
}

func transformIngress(root *yaml.Node, to string) error {
	setScalar(root, "apiVersion", to)
	spec := mapAt(root, "spec")
	if spec == nil {
		return fmt.Errorf("ingress has no spec")
	}

	// spec.backend -> spec.defaultBackend, in the new backend shape.
	if b := nodeAt(spec, "backend"); b != nil {
		nb, err := convertBackend(b)
		if err != nil {
			return err
		}
		renameKey(spec, "backend", "defaultBackend")
		replaceValue(spec, "defaultBackend", nb)
	}
	if b := nodeAt(spec, "defaultBackend"); b != nil && nodeAt(b, "serviceName") != nil {
		nb, err := convertBackend(b)
		if err != nil {
			return err
		}
		replaceValue(spec, "defaultBackend", nb)
	}

	rulesNode := nodeAt(spec, "rules")
	if rulesNode == nil {
		return nil
	}
	for _, rule := range rulesNode.Content {
		http := mapAt(rule, "http")
		if http == nil {
			continue
		}
		paths := nodeAt(http, "paths")
		if paths == nil {
			continue
		}
		for _, p := range paths.Content {
			if p.Kind != yaml.MappingNode {
				continue
			}
			if scalarAt(p, "pathType") == "" {
				// ImplementationSpecific is the only value that preserves the
				// v1beta1 matching behaviour, including nginx regex paths.
				insertAfter(p, "path", "pathType", scalarNode("ImplementationSpecific"))
			}
			b := nodeAt(p, "backend")
			if b == nil {
				return fmt.Errorf("ingress path has no backend")
			}
			nb, err := convertBackend(b)
			if err != nil {
				return err
			}
			replaceValue(p, "backend", nb)
		}
	}
	return nil
}

// convertBackend turns {serviceName, servicePort} into the v1 service shape.
// An already-converted backend is returned unchanged.
func convertBackend(b *yaml.Node) (*yaml.Node, error) {
	if b.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("backend is not a mapping")
	}
	if nodeAt(b, "service") != nil || nodeAt(b, "resource") != nil {
		return b, nil
	}
	name := scalarAt(b, "serviceName")
	portNode := nodeAt(b, "servicePort")
	if name == "" || portNode == nil {
		return nil, fmt.Errorf("backend has neither serviceName/servicePort nor service")
	}

	port := newMapping()
	if portNode.Tag == "!!int" {
		addPair(port, "number", cloneScalar(portNode))
	} else {
		addPair(port, "name", cloneScalar(portNode))
	}
	svc := newMapping()
	addPair(svc, "name", scalarNode(name))
	addPair(svc, "port", port)

	out := newMapping()
	out.HeadComment = b.HeadComment
	out.LineComment = b.LineComment
	addPair(out, "service", svc)
	return out, nil
}

func transformHPA(root *yaml.Node, to string) error {
	spec := mapAt(root, "spec")
	if spec == nil {
		return fmt.Errorf("hpa has no spec")
	}
	metrics := nodeAt(spec, "metrics")
	if metrics != nil {
		for _, m := range metrics.Content {
			if err := convertMetric(m); err != nil {
				return err
			}
		}
	}
	setScalar(root, "apiVersion", to)
	return nil
}

func convertMetric(m *yaml.Node) error {
	if m.Kind != yaml.MappingNode {
		return fmt.Errorf("metric is not a mapping")
	}
	typ := scalarAt(m, "type")
	var body *yaml.Node
	switch typ {
	case "Resource":
		body = mapAt(m, "resource")
	case "Pods":
		body = mapAt(m, "pods")
	case "":
		return fmt.Errorf("metric has no type")
	default:
		// Object and External metrics changed shape in ways that depend on the
		// metric backend; a human has to look at those.
		return fmt.Errorf("metric type %q needs a manual rewrite", typ)
	}
	if body == nil {
		return fmt.Errorf("metric of type %s has no body", typ)
	}
	if nodeAt(body, "target") != nil {
		return nil // already v2-shaped
	}

	target := newMapping()
	switch {
	case nodeAt(body, "targetAverageUtilization") != nil:
		v := nodeAt(body, "targetAverageUtilization")
		addPair(target, "type", scalarNode("Utilization"))
		addPair(target, "averageUtilization", cloneScalar(v))
		deleteKey(body, "targetAverageUtilization")
	case nodeAt(body, "targetAverageValue") != nil:
		v := nodeAt(body, "targetAverageValue")
		addPair(target, "type", scalarNode("AverageValue"))
		addPair(target, "averageValue", cloneScalar(v))
		deleteKey(body, "targetAverageValue")
	default:
		return fmt.Errorf("metric of type %s has no recognised target field", typ)
	}
	addPair(body, "target", target)
	return nil
}

// --- yaml.Node helpers -------------------------------------------------------

func nodeAt(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapAt(m *yaml.Node, key string) *yaml.Node {
	n := nodeAt(m, key)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

func scalarAt(m *yaml.Node, key string) string {
	n := nodeAt(m, key)
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func setScalar(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1].Value = value
			m.Content[i+1].Tag = "!!str"
			m.Content[i+1].Style = 0
			return
		}
	}
	addPair(m, key, scalarNode(value))
}

func renameKey(m *yaml.Node, from, to string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == from {
			m.Content[i].Value = to
			return
		}
	}
}

func replaceValue(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
}

func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func addPair(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content, scalarNode(key), value)
}

// insertAfter adds key: value directly after afterKey, keeping the field order
// a reviewer expects. It appends when afterKey is absent.
func insertAfter(m *yaml.Node, afterKey, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != afterKey {
			continue
		}
		rest := append([]*yaml.Node{scalarNode(key), value}, m.Content[i+2:]...)
		m.Content = append(m.Content[:i+2:i+2], rest...)
		return
	}
	addPair(m, key, value)
}

func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func cloneScalar(n *yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: n.Tag, Value: n.Value, Style: n.Style}
}

func newMapping() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}
