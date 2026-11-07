package openapispec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// topLevelOrder puts the document's top-level members in the order a reader
// expects; anything else follows alphabetically. Below the top level every
// object's keys are sorted (encoding/json's map ordering), so the output is a
// pure function of its inputs.
var topLevelOrder = []string{"openapi", "info", "servers", "security", "tags", "externalDocs", "paths", "components"}

// encode renders the tree as indented JSON with a stable key order. HTML
// escaping is off: descriptions carry "<", ">" and "&" that should read as
// written.
func encode(tree map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(tree))
	seen := map[string]bool{}
	for _, k := range topLevelOrder {
		if _, ok := tree[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	for _, k := range sortedKeys(tree) {
		if !seen[k] {
			keys = append(keys, k)
		}
	}

	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, k := range keys {
		kb, err := marshal(k, "")
		if err != nil {
			return nil, err
		}
		vb, err := marshal(tree[k], "  ")
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", k, err)
		}
		buf.WriteString("  ")
		buf.Write(kb)
		buf.WriteString(": ")
		buf.Write(vb)
		if i < len(keys)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

func marshal(v any, prefix string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// checkRefs fails on any "$ref" that does not resolve to a location in the
// document. kin-openapi's loader resolves references too, but this names the
// referring location, which is what a maintainer needs to fix it.
func checkRefs(tree map[string]any) error {
	var dangling []string
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch t := v.(type) {
		case map[string]any:
			if ref, ok := t["$ref"].(string); ok {
				if !resolves(tree, ref) {
					dangling = append(dangling, fmt.Sprintf("%s -> %s", at, ref))
				}
			}
			for _, k := range sortedKeys(t) {
				walk(t[k], at+"/"+escape(k))
			}
		case []any:
			for i, e := range t {
				walk(e, fmt.Sprintf("%s/%d", at, i))
			}
		}
	}
	walk(tree, "")
	if len(dangling) > 0 {
		return fmt.Errorf("dangling $ref (only local #/... references are allowed):\n  %s", strings.Join(dangling, "\n  "))
	}
	return nil
}

func resolves(tree map[string]any, ref string) bool {
	if !strings.HasPrefix(ref, "#/") {
		return false
	}
	var cur any = tree
	for _, seg := range strings.Split(ref[2:], "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[seg]; !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
