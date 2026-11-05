package patch

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/moveeeax/k8s-upgrade-report/internal/rules"
	"github.com/moveeeax/k8s-upgrade-report/internal/scan"
	"go.yaml.in/yaml/v3"
)

// Result is a generated patch plus the bookkeeping needed to explain it.
type Result struct {
	Diff    string   `json:"diff"`
	Patched []string `json:"patched"` // "file:line Kind name -> newAPIVersion"
	Skipped []string `json:"skipped"` // findings that looked autofixable but were not
}

// Generate builds one unified diff covering every autofixable finding.
// Vendor-owned resources are never patched: the diff would be reverted by the
// next Helm or Argo sync.
func Generate(set *rules.Set, files []scan.File, findings []scan.Finding) (Result, error) {
	byPath := make(map[string]scan.File, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}

	var res Result
	edits := map[string][]edit{}
	for _, f := range findings {
		if !f.Autofix || f.Category != scan.CategoryAPIRemoval {
			continue
		}
		rule, ok := set.Lookup(f.Resource.APIVersion, f.Resource.Kind)
		if !ok || !rule.Autofixable() {
			continue
		}
		file, ok := byPath[f.Resource.File]
		if !ok {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s: source file not loaded", f.Resource.File))
			continue
		}
		e, err := editFor(file, f.Target(), rule)
		if err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s:%d %s %s: %v",
				f.Resource.File, f.Resource.Line, f.Resource.Kind, f.Resource.Name, err))
			continue
		}
		edits[file.Path] = append(edits[file.Path], e)
		res.Patched = append(res.Patched, fmt.Sprintf("%s:%d %s %s -> %s",
			f.Resource.File, f.Resource.Line, f.Resource.Kind, f.Resource.Name, rule.To))
	}

	paths := make([]string, 0, len(edits))
	for p := range edits {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	for _, p := range paths {
		d, err := unifiedDiff(p, byPath[p].Lines, edits[p])
		if err != nil {
			return res, err
		}
		b.WriteString(d)
	}
	res.Diff = b.String()
	sort.Strings(res.Patched)
	sort.Strings(res.Skipped)
	return res, nil
}

func editFor(file scan.File, r scan.Resource, rule rules.Rule) (edit, error) {
	if rule.Transform == rules.TransformAPIVersion {
		return apiVersionEdit(file, r, rule)
	}

	doc, err := cloneDoc(r.Doc)
	if err != nil {
		return edit{}, err
	}
	root := doc.Content[0]
	// The comment block above the first key sits before the replaced range, so
	// re-emitting it here would duplicate it in the patched file. Depending on
	// the document it hangs off the mapping or off its first key.
	root.HeadComment = ""
	if len(root.Content) > 0 {
		root.Content[0].HeadComment = ""
	}
	if err := transformDoc(string(rule.Transform), root, rule.To); err != nil {
		return edit{}, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return edit{}, err
	}
	if err := enc.Close(); err != nil {
		return edit{}, err
	}
	newLines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")

	start, end := r.StartLine, docEnd(file, r)
	if start < 1 || end > len(file.Lines) || end < start {
		return edit{}, fmt.Errorf("document line range %d-%d is outside the file", start, end)
	}
	return edit{start: start, end: end, lines: newLines, findingID: rule.ID}, nil
}

// apiVersionEdit rewrites the single apiVersion line, so the diff stays one
// line instead of a whole re-serialised document.
func apiVersionEdit(file scan.File, r scan.Resource, rule rules.Rule) (edit, error) {
	end := docEnd(file, r)
	for i := r.StartLine; i <= end && i <= len(file.Lines); i++ {
		line := file.Lines[i-1]
		if !strings.HasPrefix(line, "apiVersion:") {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(line, "apiVersion:")) != r.APIVersion {
			continue
		}
		return edit{start: i, end: i, lines: []string{"apiVersion: " + rule.To}, findingID: rule.ID}, nil
	}
	return edit{}, fmt.Errorf("could not locate the apiVersion line for %s", r.Ref())
}

// docEnd trims trailing blank lines, comments and document separators off the
// parsed range, so re-serialising a document does not eat the header comment
// of the next one.
func docEnd(file scan.File, r scan.Resource) int {
	end := r.EndLine
	if end > len(file.Lines) {
		end = len(file.Lines)
	}
	for end > r.StartLine {
		t := strings.TrimSpace(file.Lines[end-1])
		if t == "" || t == "---" || strings.HasPrefix(t, "#") {
			end--
			continue
		}
		break
	}
	return end
}

func cloneDoc(doc *yaml.Node) (*yaml.Node, error) {
	if doc == nil || len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc.Content[0]); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	var out yaml.Node
	if err := yaml.Unmarshal(buf.Bytes(), &out); err != nil {
		return nil, err
	}
	if len(out.Content) == 0 {
		return nil, fmt.Errorf("document did not survive a round trip")
	}
	return &out, nil
}
