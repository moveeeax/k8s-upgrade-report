// Package scan reads Kubernetes manifests off disk and turns them into
// findings against the removal dataset.
package scan

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Resource is a single YAML document that looks like a Kubernetes object.
type Resource struct {
	File        string // path as given to the scanner, slash-separated
	DocIndex    int    // 0-based index of the document inside the file
	StartLine   int    // 1-based line of the first line of the document body
	EndLine     int    // 1-based line of the last line of the document body
	APIVersion  string
	Kind        string
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	Doc         *yaml.Node // the document node, comments preserved
}

// Ref is a short human identifier for a resource.
func (r Resource) Ref() string {
	ns := r.Namespace
	if ns == "" {
		ns = "-"
	}
	return fmt.Sprintf("%s %s/%s", r.Kind, ns, r.Name)
}

// File is one manifest file plus every object parsed out of it.
type File struct {
	Path      string
	Lines     []string
	Resources []Resource
}

var skipDirs = map[string]bool{
	".git": true, ".terraform": true, "node_modules": true, ".idea": true, "vendor": true,
}

// LoadDir walks root and parses every .yaml/.yml/.json manifest it can.
// Helm templates (files containing Go template actions) are skipped and
// reported separately: they are not valid YAML until rendered, and guessing at
// them produces exactly the false positives this tool exists to avoid.
func LoadDir(root string) ([]File, []string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		f, skipped, err := loadFile(root, filepath.Base(root))
		if err != nil {
			return nil, nil, err
		}
		if f == nil {
			return nil, skipped, nil
		}
		return []File{*f}, skipped, nil
	}

	var files []File
	var skipped []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(d.Name())) {
		case ".yaml", ".yml", ".json":
		default:
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		f, sk, err := loadFile(path, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		skipped = append(skipped, sk...)
		if f != nil {
			files = append(files, *f)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	sort.Strings(skipped)
	return files, skipped, nil
}

func loadFile(path, display string) (*File, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if isHelmTemplate(raw) {
		return nil, []string{display + ": unrendered Helm template"}, nil
	}
	f := &File{Path: display, Lines: splitLines(raw)}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for i := 0; ; i++ {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, []string{fmt.Sprintf("%s: %v", display, err)}, nil
		}
		res, ok := resourceFromDoc(&doc, display, i, len(f.Lines))
		if !ok {
			continue
		}
		f.Resources = append(f.Resources, res)
	}
	if len(f.Resources) == 0 {
		return nil, nil, nil
	}
	// A document ends where the next one starts.
	for i := 0; i < len(f.Resources)-1; i++ {
		f.Resources[i].EndLine = f.Resources[i+1].StartLine - 1
	}
	return f, nil, nil
}

func resourceFromDoc(doc *yaml.Node, file string, idx, totalLines int) (Resource, bool) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return Resource{}, false
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return Resource{}, false
	}
	apiVersion := scalarAt(root, "apiVersion")
	kind := scalarAt(root, "kind")
	if apiVersion == "" || kind == "" {
		return Resource{}, false
	}
	meta := mapAt(root, "metadata")
	res := Resource{
		File:        file,
		DocIndex:    idx,
		StartLine:   root.Line,
		EndLine:     totalLines,
		APIVersion:  apiVersion,
		Kind:        kind,
		Name:        scalarAt(meta, "name"),
		Namespace:   scalarAt(meta, "namespace"),
		Labels:      stringMapAt(meta, "labels"),
		Annotations: stringMapAt(meta, "annotations"),
		Doc:         doc,
	}
	if res.Name == "" {
		res.Name = scalarAt(meta, "generateName")
	}
	if res.Name == "" {
		res.Name = "(unnamed)"
	}
	return res, true
}

func isHelmTemplate(raw []byte) bool {
	return bytes.Contains(raw, []byte("{{")) && bytes.Contains(raw, []byte("}}"))
}

func splitLines(raw []byte) []string {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// --- small yaml.Node helpers -------------------------------------------------

// MapAt returns the mapping value stored under key, or nil.
func MapAt(m *yaml.Node, key string) *yaml.Node { return mapAt(m, key) }

// ScalarAt returns the scalar value stored under key, or "".
func ScalarAt(m *yaml.Node, key string) string { return scalarAt(m, key) }

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

func stringMapAt(m *yaml.Node, key string) map[string]string {
	n := mapAt(m, key)
	if n == nil {
		return nil
	}
	out := map[string]string{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i+1].Kind == yaml.ScalarNode {
			out[n.Content[i].Value] = n.Content[i+1].Value
		}
	}
	return out
}
