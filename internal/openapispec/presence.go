package openapispec

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Presence and nullability: which properties are always in the JSON, and which
// can be null. Plus the descriptions protoc-gen-openapiv2 puts beside a $ref.
//
// The gateway marshals with protojson EmitUnpopulated, which writes every
// field of a message except an unset oneof member. An unset field with
// presence is JSON null: every singular message-typed field, including
// google.protobuf.Timestamp and the wrapper types. Scalars, enums, repeated
// fields and maps have no presence and serialize as their zero value ("", 0,
// false, the 0 enum name, [], {}), never null. Members of a oneof are
// different again: unset, they are left out, not null.
//
// So the wire gives every non-oneof field a key (AlwaysPresent), and the spec
// lists each in its schema's `required`. Whether the value can be null is the
// one thing the marshaler cannot decide, because it depends on whether the
// server ever leaves the field unset. A field the server always populates is
// annotated `(google.api.field_behavior) = REQUIRED` in grid.proto; it is
// required and NOT nullable. Every other message-typed field is nullable
// (Nullable). TestResponsesHonorFieldPresence (internal/gridapi) and
// TestNormalizersHonorFieldPresence (internal/ingest) hold the annotation to
// what the code really produces, using CheckWire.
//
// Swagger 2.0 cannot say any of that, and a wrapper (Int32Value) is generated
// as a plain integer indistinguishable from int32, so the swagger alone cannot
// tell which properties are nullable. The proto descriptor can: presenceFields
// derives both sets from it, and markPresence writes `nullable: true` and
// `required` onto the converted 3.0 schemas.
//
// A $ref property cannot simply gain `nullable`: in 3.0 a Reference Object's
// siblings are ignored. It becomes `allOf: [{$ref}]` with `nullable: true`
// and the referenced schema's `type` beside it (3.0.3 ignores a `nullable`
// without a `type`). keepRefSiblings applies the same wrapping, before conversion, to
// every $ref property that carries a description or example, because the
// 2.0-to-3.0 conversion would otherwise drop them (the enum-typed `status`
// and `layer` fields lost their documentation that way). A REQUIRED message
// field with a description therefore keeps a one-element allOf (and no
// `type` or `nullable`); one without stays a bare $ref.

// MarkedRequired reports whether f carries `(google.api.field_behavior) =
// REQUIRED`: grid.proto's statement that the server always sets it.
func MarkedRequired(f protoreflect.FieldDescriptor) bool {
	opts := f.Options()
	if opts == nil || !proto.HasExtension(opts, annotations.E_FieldBehavior) {
		return false
	}
	behaviors, _ := proto.GetExtension(opts, annotations.E_FieldBehavior).([]annotations.FieldBehavior)
	return slices.Contains(behaviors, annotations.FieldBehavior_REQUIRED)
}

// AlwaysPresent reports whether the gateway's JSON always has a key for f:
// every field except a oneof member (a proto3 `optional` is a synthetic oneof),
// which is left out when unset (encoding/protojson unpopulatedFieldRanger).
func AlwaysPresent(f protoreflect.FieldDescriptor) bool {
	return f.ContainingOneof() == nil
}

// Nullable reports whether the gateway's JSON can carry null for f: a singular
// message-typed field outside a oneof that grid.proto does not mark REQUIRED.
func Nullable(f protoreflect.FieldDescriptor) bool {
	return emitsNull(f) && !MarkedRequired(f)
}

// emitsNull reports whether protojson with EmitUnpopulated writes f as null
// when it is unset (encoding/protojson unpopulatedFieldRanger).
func emitsNull(f protoreflect.FieldDescriptor) bool {
	return f.HasPresence() && f.ContainingOneof() == nil && !f.IsList() && !f.IsMap()
}

// presence is one schema's property lists, as JSON names in field order.
type presence struct {
	required []string // AlwaysPresent
	nullable []string // Nullable
}

// presenceFields maps each generated schema name to its required and nullable
// properties. Schema names follow protoc-gen-openapiv2's default (legacy)
// naming: the package's last component followed by the message name, nested
// names concatenated (`v1Source`).
func presenceFields(fd protoreflect.FileDescriptor) map[string]presence {
	out := map[string]presence{}
	if fd == nil {
		return out
	}
	pkg := string(fd.Package())
	prefix := pkg[strings.LastIndex(pkg, ".")+1:]
	var walk func(msgs protoreflect.MessageDescriptors, name string)
	walk = func(msgs protoreflect.MessageDescriptors, name string) {
		for i := 0; i < msgs.Len(); i++ {
			md := msgs.Get(i)
			if md.IsMapEntry() {
				continue
			}
			schema := name + string(md.Name())
			var p presence
			fields := md.Fields()
			for j := 0; j < fields.Len(); j++ {
				f := fields.Get(j)
				if AlwaysPresent(f) {
					p.required = append(p.required, f.JSONName())
				}
				if Nullable(f) {
					p.nullable = append(p.nullable, f.JSONName())
				}
			}
			if len(p.required) > 0 {
				out[schema] = p
			}
			walk(md.Messages(), schema)
		}
	}
	walk(fd.Messages(), prefix)
	return out
}

// markPresence writes each schema's `required` list and sets `nullable: true`
// on its nullable properties. A schema the generator did not emit (a request
// message, which is never a body) is skipped; a listed property missing from
// an emitted schema is an error, because it means the naming rule above no
// longer matches the generator's and the markers would silently go missing.
func markPresence(doc map[string]any, fields map[string]presence) error {
	schemas, _ := lookup(doc, "components", "schemas").(map[string]any)
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		schema, ok := schemas[name].(map[string]any)
		if !ok {
			continue
		}
		props, _ := schema["properties"].(map[string]any)
		p := fields[name]
		for _, prop := range p.required {
			if _, ok := props[prop].(map[string]any); !ok {
				return fmt.Errorf("presence: schema %s has no property %q (did protoc-gen-openapiv2's naming change?)", name, prop)
			}
		}
		schema["required"] = mergeRequired(schema["required"], p.required)
		for _, prop := range p.nullable {
			if err := markNullable(doc, props[prop].(map[string]any)); err != nil {
				return fmt.Errorf("presence: %s.%s: %w", name, prop, err)
			}
		}
	}
	return nil
}

// mergeRequired unions the generator's `required` list (from REQUIRED
// annotations) with the derived one, sorted so the output is stable.
func mergeRequired(existing any, derived []string) []any {
	set := map[string]bool{}
	if list, ok := existing.([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				set[s] = true
			}
		}
	}
	for _, s := range derived {
		set[s] = true
	}
	keys := make([]string, 0, len(set))
	for s := range set {
		keys = append(keys, s)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, s := range keys {
		out[i] = s
	}
	return out
}

// markNullable sets `nullable: true` on one property, wrapping a $ref in allOf
// with the referenced schema's type so 3.0.3 honours it.
func markNullable(doc map[string]any, p map[string]any) error {
	if ref, isRef := p["$ref"]; isRef {
		delete(p, "$ref")
		p["allOf"] = []any{map[string]any{"$ref": ref}}
	}
	if allOf, ok := p["allOf"].([]any); ok {
		typ, err := refType(doc, allOf)
		if err != nil {
			return err
		}
		p["type"] = typ
	}
	p["nullable"] = true
	return nil
}

// refType returns the `type` of the schema a single-element allOf references.
func refType(doc map[string]any, allOf []any) (string, error) {
	if len(allOf) != 1 {
		return "", fmt.Errorf("want allOf with one $ref, have %d entries", len(allOf))
	}
	ref, _ := lookup(allOf[0], "$ref").(string)
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("unexpected $ref %q", ref)
	}
	typ, _ := lookup(doc, "components", "schemas", strings.TrimPrefix(ref, prefix), "type").(string)
	if typ == "" {
		return "", fmt.Errorf("%s has no type", ref)
	}
	return typ, nil
}

// keepRefSiblings rewrites, in the generated Swagger 2.0 tree, each definition
// property that has a $ref plus other members into `allOf: [{$ref}]` plus those
// members, so the conversion to 3.0 keeps its description and example.
func keepRefSiblings(swagger map[string]any) {
	defs, _ := swagger["definitions"].(map[string]any)
	for _, def := range defs {
		props, _ := lookup(def, "properties").(map[string]any)
		for _, v := range props {
			p, ok := v.(map[string]any)
			if !ok || len(p) < 2 {
				continue
			}
			if ref, isRef := p["$ref"]; isRef {
				delete(p, "$ref")
				p["allOf"] = []any{map[string]any{"$ref": ref}}
			}
		}
	}
}

// lookup walks nested objects by key, returning nil when any step is missing.
func lookup(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
