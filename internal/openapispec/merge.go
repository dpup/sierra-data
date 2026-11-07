package openapispec

import (
	"fmt"
	"reflect"
	"strings"
)

// The merge works on generic JSON trees and addresses everything by JSON
// pointer, so the rules can be stated per location:
//
//	Owned (overlay `spec`)
//	  - absent in the generated spec      -> added
//	  - both objects                      -> merged key by key
//	  - equal values                      -> fine
//	  - a path (/paths/{p}) or a named component (/components/{kind}/{name})
//	    that the generator already emits  -> ERROR: the route or schema is now
//	    generated; delete it from `spec` (keep only annotations, in `defaults`)
//	  - any other differing value         -> ERROR
//
//	Defaults (overlay `defaults`)
//	  - absent in the generated spec      -> added
//	  - both objects                      -> merged key by key
//	  - present in the generated spec     -> the GENERATED value wins (reported
//	    as shadowed), except a protoc-gen-openapiv2 placeholder, which is replaced
//	  - a path, an operation or a named component that the generated spec does
//	    NOT have                          -> ERROR: the annotation's target is
//	    gone (renamed RPC, removed schema), and silently creating a skeleton
//	    operation would publish a route that does not exist
//
// Arrays are values (a parameter list is replaced or kept whole) except /tags,
// which is merged by tag name so the overlay can describe a generated tag.

// keyedArrays names the arrays merged element-by-element, and the field that
// identifies an element.
var keyedArrays = map[string]string{"/tags": "name"}

// placeholders are values protoc-gen-openapiv2 emits when grid.proto says
// nothing; a default replaces them rather than being shadowed by them.
var placeholders = map[string]func(any) bool{
	"/info/title": func(v any) bool {
		s, _ := v.(string)
		return strings.HasSuffix(s, ".proto")
	},
	"/info/version": func(v any) bool { return v == "version not set" },
}

func mergeOwned(base, add map[string]any, ptr string) error {
	for _, k := range sortedKeys(add) {
		v := add[k]
		p := ptr + "/" + escape(k)
		cur, exists := base[k]
		if !exists {
			base[k] = v
			continue
		}
		if isEntity(p) {
			return fmt.Errorf("%s is already in the generated spec; remove it from the overlay's spec section (annotate it under defaults instead)", p)
		}
		if field, ok := keyedArrays[p]; ok {
			merged, err := mergeKeyed(cur, v, p, field, func(b, a map[string]any, ep string) error {
				return mergeOwned(b, a, ep)
			})
			if err != nil {
				return err
			}
			base[k] = merged
			continue
		}
		bm, bok := cur.(map[string]any)
		am, aok := v.(map[string]any)
		if bok && aok {
			if err := mergeOwned(bm, am, p); err != nil {
				return err
			}
			continue
		}
		if !reflect.DeepEqual(cur, v) {
			return fmt.Errorf("%s: overlay value conflicts with the generated spec (generated %s, overlay %s)", p, short(cur), short(v))
		}
	}
	return nil
}

func mergeDefaults(base, add map[string]any, ptr string, rep *Report) error {
	for _, k := range sortedKeys(add) {
		v := add[k]
		p := ptr + "/" + escape(k)
		cur, exists := base[k]
		if !exists {
			if isEntity(p) || isOperation(p) {
				return fmt.Errorf("%s does not exist in the generated spec; a default can only annotate generated content (was the RPC or message renamed?)", p)
			}
			base[k] = v
			continue
		}
		if field, ok := keyedArrays[p]; ok {
			merged, err := mergeKeyed(cur, v, p, field, func(b, a map[string]any, ep string) error {
				return mergeDefaults(b, a, ep, rep)
			})
			if err != nil {
				return err
			}
			base[k] = merged
			continue
		}
		bm, bok := cur.(map[string]any)
		am, aok := v.(map[string]any)
		if bok && aok {
			if err := mergeDefaults(bm, am, p, rep); err != nil {
				return err
			}
			continue
		}
		if isPlaceholder, ok := placeholders[p]; ok && isPlaceholder(cur) {
			base[k] = v
			continue
		}
		if !reflect.DeepEqual(cur, v) {
			rep.Shadowed = append(rep.Shadowed, p)
		}
	}
	return nil
}

// mergeKeyed merges two arrays of objects by an identifying field: elements are
// matched by that field and merged with fn, new elements are appended in overlay
// order, and generated elements keep their position.
func mergeKeyed(cur, add any, ptr, field string, fn func(b, a map[string]any, ptr string) error) ([]any, error) {
	ba, ok1 := cur.([]any)
	aa, ok2 := add.([]any)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("%s: expected arrays on both sides", ptr)
	}
	index := map[string]map[string]any{}
	for _, e := range ba {
		if m, ok := e.(map[string]any); ok {
			if id, ok := m[field].(string); ok {
				index[id] = m
			}
		}
	}
	out := ba
	for i, e := range aa {
		m, ok := e.(map[string]any)
		id, idOK := m[field].(string)
		if !ok || !idOK {
			return nil, fmt.Errorf("%s/%d: element needs a string %q", ptr, i, field)
		}
		if b, found := index[id]; found {
			if err := fn(b, m, ptr+"/"+escape(id)); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, m)
		index[id] = m
	}
	return out, nil
}

// isEntity reports whether ptr names a path item or a named component, the
// units the overlay either owns outright or merely annotates.
func isEntity(ptr string) bool {
	seg := strings.Split(ptr, "/")[1:]
	return (len(seg) == 2 && seg[0] == "paths") ||
		(len(seg) == 3 && seg[0] == "components")
}

// isOperation reports whether ptr names an operation within a path item.
func isOperation(ptr string) bool {
	seg := strings.Split(ptr, "/")[1:]
	if len(seg) != 3 || seg[0] != "paths" {
		return false
	}
	switch seg[2] {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	}
	return false
}

// escape encodes a key as a JSON pointer segment (RFC 6901).
func escape(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}

func short(v any) string {
	s := fmt.Sprintf("%v", v)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return fmt.Sprintf("%q", s)
}
