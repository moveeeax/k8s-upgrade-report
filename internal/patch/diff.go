package patch

import (
	"fmt"
	"sort"
	"strings"
)

// edit replaces the inclusive 1-based line range [start,end] of a file.
type edit struct {
	start, end int
	lines      []string
	findingID  string
}

const contextLines = 3

// unifiedDiff renders the edits against the original lines as a unified diff
// with a/ and b/ prefixes, so that `git apply` accepts it with default -p1.
func unifiedDiff(path string, orig []string, edits []edit) (string, error) {
	if len(edits) == 0 {
		return "", nil
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	for i := 1; i < len(edits); i++ {
		if edits[i].start <= edits[i-1].end {
			return "", fmt.Errorf("%s: overlapping edits at line %d", path, edits[i].start)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)

	offset := 0 // running new-file line offset from earlier hunks
	for _, group := range groupEdits(edits) {
		first, last := group[0], group[len(group)-1]
		ctxStart := max(1, first.start-contextLines)
		ctxEnd := min(len(orig), last.end+contextLines)

		var body []string
		oldCount, newCount := 0, 0
		cur := ctxStart
		for _, e := range group {
			for ; cur < e.start; cur++ {
				body = append(body, " "+orig[cur-1])
				oldCount++
				newCount++
			}
			for ; cur <= e.end; cur++ {
				body = append(body, "-"+orig[cur-1])
				oldCount++
			}
			for _, l := range e.lines {
				body = append(body, "+"+l)
				newCount++
			}
		}
		for ; cur <= ctxEnd; cur++ {
			body = append(body, " "+orig[cur-1])
			oldCount++
			newCount++
		}

		fmt.Fprintf(&b, "@@ -%s +%s @@\n", rangeSpec(ctxStart, oldCount), rangeSpec(ctxStart+offset, newCount))
		for _, l := range body {
			b.WriteString(l + "\n")
		}
		offset += newCount - oldCount
	}
	return b.String(), nil
}

// groupEdits merges edits whose context windows touch, so the patch does not
// emit two hunks that overlap.
func groupEdits(edits []edit) [][]edit {
	var groups [][]edit
	cur := []edit{edits[0]}
	for _, e := range edits[1:] {
		if e.start-cur[len(cur)-1].end <= 2*contextLines+1 {
			cur = append(cur, e)
			continue
		}
		groups = append(groups, cur)
		cur = []edit{e}
	}
	return append(groups, cur)
}

func rangeSpec(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start-1)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
