package openapispec

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// Nullability, and the descriptions protoc-gen-openapiv2 puts beside a $ref.
//
// The gateway marshals with protojson EmitUnpopulated, which writes an unset
// field with presence as JSON null: every singular message-typed field,
// including google.protobuf.Timestamp and the wrapper types. Scalars, enums,
// repeated fields and maps have no presence and serialize as their zero value
// ("", 0, false, the 0 enum name, [], {}), never null. Members of a oneof are
// different again: unset, they are left out, not null.
//
// Swagger 2.0 cannot say any of that, and a wrapper (Int32Value) is generated
// as a plain integer indistinguishable from int32, so the swagger alone cannot
// tell which properties are nullable. The proto descriptor can: nullableFields
// derives the set from it, and markNullable writes `nullable: true` onto the
// converted 3.0 schemas.
//
// A $ref property cannot simply gain `nullable`: in 3.0 a Reference Object's
// siblings are ignored. It becomes `allOf: [{$ref}]` with `nullable: true`
// and the referenced schema's `type` beside it (3.0.3 ignores a `nullable`
// without a `type`). keepRefSiblings applies the same wrapping, before conversion, to
// every $ref property that carries a description or example, because the
// 2.0-to-3.0 conversion would otherwise drop them (the enum-typed `status`
// and `layer` fields lost their documentation that way).

// nullableFields maps each generated schema name to the JSON names of its
// properties that can serialize as null. Schema names follow
// protoc-gen-openapiv2's default (legacy) naming: the package's last component
// followed by the message name, nested names concatenated (`v1Source`).
func nullableFields(fd protoreflect.FileDescriptor) map[string][]string {
	out := map[string][]string{}
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
			fields := md.Fields()
			for j := 0; j < fields.Len(); j++ {
				if f := fields.Get(j); emitsNull(f) {
					out[schema] = append(out[schema], f.JSONName())
				}
			}
			walk(md.Messages(), schema)
		}
	}
	walk(fd.Messages(), prefix)
	return out
}

// emitsNull reports whether protojson with EmitUnpopulated writes f as null
// when it is unset (encoding/protojson unpopulatedFieldRanger).
func emitsNull(f protoreflect.FieldDescriptor) bool {
	return f.HasPresence() && f.ContainingOneof() == nil && !f.IsList() && !f.IsMap()
}

// markNullable sets `nullable: true` on each listed property of the converted
// document. A schema the generator did not emit (a request message, which is
// never a body) is skipped; a listed property missing from an emitted schema
// is an error, because it means the naming rule above no longer matches the
// generator's and the nullability would silently go missing.
func markNullable(doc map[string]any, fields map[string][]string) error {
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
		for _, prop := range fields[name] {
			p, ok := props[prop].(map[string]any)
			if !ok {
				return fmt.Errorf("nullable: schema %s has no property %q (did protoc-gen-openapiv2's naming change?)", name, prop)
			}
			if ref, isRef := p["$ref"]; isRef {
				delete(p, "$ref")
				p["allOf"] = []any{map[string]any{"$ref": ref}}
			}
			if allOf, ok := p["allOf"].([]any); ok {
				// 3.0.3 gives `nullable` meaning only beside a `type`, so the
				// wrapper states the referenced schema's type.
				typ, err := refType(doc, allOf)
				if err != nil {
					return fmt.Errorf("nullable: %s.%s: %w", name, prop, err)
				}
				p["type"] = typ
			}
			p["nullable"] = true
		}
	}
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
