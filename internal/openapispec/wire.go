package openapispec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// CheckWire checks one JSON body, as the gateway marshaled a message of type
// md, against the presence contract the spec publishes (presence.go): every
// AlwaysPresent field has a key, and no field that is not Nullable is null. It
// descends into nested messages, lists and map values, and returns one line
// per violation, each naming the JSON path.
//
// seen, when non-nil, records the full name of each REQUIRED-marked field
// observed with a value ("grid.v1.Place.geometry"), so a test can prove its
// fixtures exercised every annotation (see RequiredFieldsReachable).
//
// It exists so the REQUIRED annotations in grid.proto cannot drift from what
// the code produces: the contract tests marshal real responses and run them
// through here.
func CheckWire(md protoreflect.MessageDescriptor, body []byte, seen map[string]bool) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("check wire: decode %s: %w", md.FullName(), err)
	}
	var problems []string
	checkMessage(md, v, string(md.Name()), &problems, seen)
	return problems, nil
}

// RequiredFieldsReachable returns the full names of every REQUIRED-marked
// field in a message reachable from roots, sorted.
func RequiredFieldsReachable(roots ...protoreflect.MessageDescriptor) []string {
	visited := map[protoreflect.FullName]bool{}
	set := map[string]bool{}
	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if md == nil || visited[md.FullName()] || isWellKnown(md) {
			return
		}
		visited[md.FullName()] = true
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if MarkedRequired(f) {
				set[string(f.FullName())] = true
			}
			if f.IsMap() {
				walk(f.MapValue().Message())
			} else {
				walk(f.Message())
			}
		}
	}
	for _, r := range roots {
		walk(r)
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func checkMessage(md protoreflect.MessageDescriptor, v any, path string, problems *[]string, seen map[string]bool) {
	if isWellKnown(md) {
		return // Timestamp, wrappers: protojson writes them as scalars
	}
	obj, ok := v.(map[string]any)
	if !ok {
		*problems = append(*problems, fmt.Sprintf("%s: want a JSON object for %s, have %T", path, md.FullName(), v))
		return
	}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		at := path + "." + f.JSONName()
		val, present := obj[f.JSONName()]
		switch {
		case !present:
			if AlwaysPresent(f) {
				*problems = append(*problems, at+": missing")
			}
			continue
		case val == nil:
			if !Nullable(f) {
				*problems = append(*problems, at+": null, but the spec says it never is")
			}
			continue
		}
		if seen != nil && MarkedRequired(f) {
			seen[string(f.FullName())] = true
		}
		checkValue(f, val, at, problems, seen)
	}
}

// checkValue descends into a present, non-null field value.
func checkValue(f protoreflect.FieldDescriptor, val any, at string, problems *[]string, seen map[string]bool) {
	switch {
	case f.IsMap():
		vmd := f.MapValue().Message()
		m, _ := val.(map[string]any)
		if vmd == nil {
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			checkMessage(vmd, m[k], at+"["+k+"]", problems, seen)
		}
	case f.IsList():
		if f.Message() == nil {
			return
		}
		list, _ := val.([]any)
		for i, elem := range list {
			checkMessage(f.Message(), elem, fmt.Sprintf("%s[%d]", at, i), problems, seen)
		}
	case f.Message() != nil:
		checkMessage(f.Message(), val, at, problems, seen)
	}
}

// isWellKnown reports whether md is a google.protobuf type, which protojson
// renders in its own JSON form rather than as an object of its fields.
func isWellKnown(md protoreflect.MessageDescriptor) bool {
	return strings.HasPrefix(string(md.FullName()), "google.protobuf.")
}
