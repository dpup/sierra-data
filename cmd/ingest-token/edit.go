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
		if start, end, ok := findEntry(lines, listStart, listEnd, itemIndent, r.ID); ok {
			updated, err := replaceTokenHash(lines, start, end, itemIndent, r.TokenSha256)
			if err != nil {
				return "", "", err
			}
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

// findEntry locates an existing `- id: <id>` entry and returns its line range.
func findEntry(lines []string, start, end, itemIndent int, id string) (int, int, bool) {
	prefix := strings.Repeat(" ", itemIndent) + "- "
	entryStart := -1
	for i := start; i < end; i++ {
		if !strings.HasPrefix(lines[i], prefix) {
			continue
		}
		if entryStart >= 0 {
			return entryStart, i, true // previous entry ended here
		}
		if entryID(lines, i, end, itemIndent) == id {
			entryStart = i
		}
	}
	if entryStart >= 0 {
		return entryStart, end, true
	}
	return 0, 0, false
}

// entryID reads the `id:` of the entry beginning at start.
func entryID(lines []string, start, end, itemIndent int) string {
	fieldIndent := strings.Repeat(" ", itemIndent+2)
	itemPrefix := strings.Repeat(" ", itemIndent) + "- "
	for i := start; i < end; i++ {
		l := lines[i]
		if i > start && strings.HasPrefix(l, itemPrefix) {
			break // next entry
		}
		trimmed := strings.TrimSpace(strings.TrimPrefix(l, itemPrefix))
		if i > start && !strings.HasPrefix(l, fieldIndent) {
			break
		}
		if v, ok := strings.CutPrefix(trimmed, "id:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// replaceTokenHash rewrites the tokenSha256 of an existing entry in place,
// adding the field if the entry somehow lacks one.
func replaceTokenHash(lines []string, start, end, itemIndent int, hash string) ([]string, error) {
	out := append([]string(nil), lines...)
	fieldIndent := strings.Repeat(" ", itemIndent+2)
	for i := start; i < end; i++ {
		trimmed := strings.TrimSpace(strings.TrimPrefix(out[i], strings.Repeat(" ", itemIndent)+"- "))
		if strings.HasPrefix(trimmed, "tokenSha256:") {
			out[i] = fieldIndent + `tokenSha256: "` + hash + `"`
			return out, nil
		}
	}
	// No hash to replace: put one directly after the id line so the entry ends
	// up valid rather than half-configured.
	for i := start; i < end; i++ {
		trimmed := strings.TrimSpace(strings.TrimPrefix(out[i], strings.Repeat(" ", itemIndent)+"- "))
		if strings.HasPrefix(trimmed, "id:") {
			ins := fieldIndent + `tokenSha256: "` + hash + `"`
			out = append(out[:i+1], append([]string{ins}, out[i+1:]...)...)
			return out, nil
		}
	}
	return nil, fmt.Errorf("existing reporter entry has no `id:` field")
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
