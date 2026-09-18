package main

import (
	"fmt"
	"strings"
)

// action says what upsertReporter did, for the operator-facing summary.
type action string

const (
	actionAdded   action = "added"
	actionRotated action = "rotated"
)

// reporterSpec is one entry to write into grid.ingest.reporters.
type reporterSpec struct {
	ID          string
	Name        string
	Streams     []string
	TokenSha256 string
	StaleAfter  string
	MinInterval string
	Priority    int
}

// upsertReporter inserts (or rotates the token of) a reporter inside
// prefab.yaml's grid.ingest.reporters list, returning the new file content.
//
// It edits the file as TEXT rather than round-tripping it through a YAML
// library, and that is the whole design constraint. prefab.yaml is roughly half
// prose — every non-obvious value carries the reasoning for its setting, and
// several of those comments are the only record of why a knob may not be
// changed. A marshal/unmarshal cycle would delete all of it and reflow
// everything that survived, turning a one-line credential change into an
// unreviewable diff. Standard YAML libraries cannot round-trip comments; the
// ones that can are not dependencies worth taking for this.
//
// So: find the list, splice one entry, leave every other byte alone.
func upsertReporter(src string, r reporterSpec) (string, action, error) {
	lines := strings.Split(src, "\n")

	keyIdx, keyIndent, empty, err := findReportersKey(lines)
	if err != nil {
		return "", "", err
	}

	itemIndent := keyIndent + 2
	listStart := keyIdx + 1
	listEnd := listStart // exclusive
	if !empty {
		listEnd = listExtent(lines, listStart, itemIndent)
	}

	// Rotation: an existing entry with this id keeps its position and every
	// other setting — only the hash moves. Deleting and re-appending would
	// silently discard a hand-tuned staleAfter or placeIds, which is exactly
	// what someone rotating a credential is not expecting to happen.
	if !empty {
		if updated, rotated := rotateToken(lines, listStart, listEnd, itemIndent, r.ID, r.TokenSha256); rotated {
			return strings.Join(updated, "\n"), actionRotated, nil
		}
	}

	entry := renderEntry(r, itemIndent)
	out := make([]string, 0, len(lines)+len(entry))
	out = append(out, lines[:keyIdx]...)
	if empty {
		// `reporters: []` has to become a block list before anything can be
		// nested under it.
		out = append(out, strings.Repeat(" ", keyIndent)+"reporters:")
	} else {
		out = append(out, lines[keyIdx])
	}
	out = append(out, lines[keyIdx+1:listEnd]...)
	out = append(out, entry...)
	out = append(out, lines[listEnd:]...)
	return strings.Join(out, "\n"), actionAdded, nil
}

// findReportersKey locates `reporters:` inside the `ingest:` block. Both
// anchors are required: `reporters` is a plausible key name elsewhere, and
// splicing a credential into the wrong section would be a silent, dangerous
// success. Failing loudly here is the point.
func findReportersKey(lines []string) (idx, indent int, empty bool, err error) {
	ingest := -1
	for i, l := range lines {
		if strings.TrimRight(l, " \t") == "  ingest:" {
			if ingest >= 0 {
				return 0, 0, false, fmt.Errorf("prefab.yaml has more than one `ingest:` block; refusing to guess")
			}
			ingest = i
		}
	}
	if ingest < 0 {
		return 0, 0, false, fmt.Errorf("could not find the `grid.ingest:` block in prefab.yaml")
	}

	for i := ingest + 1; i < len(lines); i++ {
		l := lines[i]
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ind := leadingSpaces(l)
		if ind <= 2 {
			break // left the ingest block
		}
		rest, ok := strings.CutPrefix(trimmed, "reporters:")
		if !ok {
			continue
		}
		switch v := strings.TrimSpace(rest); v {
		case "":
			return i, ind, false, nil
		case "[]":
			return i, ind, true, nil
		default:
			return 0, 0, false, fmt.Errorf(
				"grid.ingest.reporters is %q; this tool only edits an empty `[]` or a block list", v)
		}
	}
	return 0, 0, false, fmt.Errorf("could not find `reporters:` inside the `grid.ingest:` block")
}

// listExtent returns the index just past the last line belonging to the list.
// Anything indented at least as far as a list item is part of it; the commented
// example that follows the list in prefab.yaml sits at the key's indent, so it
// is correctly excluded.
func listExtent(lines []string, start, itemIndent int) int {
	end := start
	for i := start; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			break
		}
		if leadingSpaces(lines[i]) < itemIndent {
			break
		}
		end = i + 1
	}
	return end
}

// entryRanges splits the list region into one [start, end) range per item.
// Everything indented at least as far as a field line belongs to the item above
// it, comments included — which is what lets a rotation preserve an operator's
// hand-written note on the entry it is rewriting.
func entryRanges(lines []string, start, end, itemIndent int) [][2]int {
	itemPrefix := strings.Repeat(" ", itemIndent) + "- "
	var out [][2]int
	for i := start; i < end; i++ {
		if !strings.HasPrefix(lines[i], itemPrefix) {
			continue
		}
		if n := len(out); n > 0 {
			out[n-1][1] = i
		}
		out = append(out, [2]int{i, end})
	}
	return out
}

// entryField finds a scalar field inside one entry, returning its line index and
// unquoted value. The `- ` of the first line is stripped so the item's inline
// field (`- id: alan-pi`) reads the same as an indented one.
func entryField(lines []string, r [2]int, itemIndent int, name string) (int, string, bool) {
	itemPrefix := strings.Repeat(" ", itemIndent) + "- "
	for i := r[0]; i < r[1]; i++ {
		trimmed := strings.TrimSpace(strings.TrimPrefix(lines[i], itemPrefix))
		if v, ok := strings.CutPrefix(trimmed, name+":"); ok {
			return i, strings.Trim(strings.TrimSpace(v), `"'`), true
		}
	}
	return 0, "", false
}

// rotateToken rewrites an existing reporter's hash in place, leaving every other
// line of the entry untouched. Rebuilding the entry instead would silently drop
// a hand-tuned staleAfter or placeIds, which is not what someone rotating a
// credential is asking for.
func rotateToken(lines []string, start, end, itemIndent int, id, hash string) ([]string, bool) {
	fieldIndent := strings.Repeat(" ", itemIndent+2)
	for _, r := range entryRanges(lines, start, end, itemIndent) {
		if _, got, ok := entryField(lines, r, itemIndent, "id"); !ok || got != id {
			continue
		}
		out := append([]string(nil), lines...)
		line := fieldIndent + `tokenSha256: "` + hash + `"`
		if i, _, ok := entryField(out, r, itemIndent, "tokenSha256"); ok {
			out[i] = line
			return out, true
		}
		// No hash to replace: put one after the id so the entry ends up valid
		// rather than half-configured.
		i, _, _ := entryField(out, r, itemIndent, "id")
		return append(out[:i+1], append([]string{line}, out[i+1:]...)...), true
	}
	return nil, false
}

// renderEntry writes a new reporter block. The comments are deliberate: this
// file is read far more often than it is written, and the two settings most
// likely to be changed later are the two whose consequences are least guessable
// from their names.
func renderEntry(r reporterSpec, itemIndent int) []string {
	item := strings.Repeat(" ", itemIndent)
	field := strings.Repeat(" ", itemIndent+2)
	quoted := make([]string, len(r.Streams))
	for i, s := range r.Streams {
		quoted[i] = `"` + s + `"`
	}
	return []string{
		item + "- id: " + r.ID,
		field + `name: "` + yamlEscape(r.Name) + `"`,
		field + `tokenSha256: "` + r.TokenSha256 + `"`,
		field + "streams: [" + strings.Join(quoted, ", ") + "]",
		field + "# Silence past this degrades this row on /api/v1/sources AND suppresses",
		field + "# the mesh disappearance sweep (absence must not read as departure).",
		field + `staleAfter: "` + r.StaleAfter + `"`,
		field + `minInterval: "` + r.MinInterval + `"`,
		field + "# Breaks ties when two monitors disagree about a node's identity.",
		field + fmt.Sprintf("priority: %d", r.Priority),
	}
}

// yamlEscape makes a value safe inside a double-quoted YAML scalar.
func yamlEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func leadingSpaces(s string) int {
	for i, r := range s {
		if r != ' ' {
			return i
		}
	}
	return len(s)
}
