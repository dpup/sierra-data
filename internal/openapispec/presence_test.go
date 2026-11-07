package openapispec

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	_ "google.golang.org/protobuf/types/known/timestamppb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"
)

// presenceFixtureProto is one message with a field of every shape the gateway
// marshals differently under EmitUnpopulated, plus the two REQUIRED shapes:
//
//	message Thing {
//	  string name = 1;                         // scalar: ""      -> required, not nullable
//	  Kind kind = 2;                           // enum: 0 name    -> required, not nullable
//	  google.protobuf.Timestamp seen_at = 3;   // message: null   -> required, nullable
//	  google.protobuf.Int32Value count = 4;    // wrapper: null   -> required, nullable
//	  Other other = 5;                         // message: null   -> required, nullable
//	  repeated Other others = 6;               // list: []        -> required, not nullable
//	  map<string, int32> tally = 7;            // map: {}         -> required, not nullable
//	  oneof detail { Other a = 8; Other b = 9; } // absent, not null -> neither
//	  optional int32 opt = 10;                 // proto3 optional: absent -> neither
//	  Other must = 11 [(google.api.field_behavior) = REQUIRED];                    // required, not nullable
//	  google.protobuf.Timestamp made_at = 12 [(google.api.field_behavior) = REQUIRED]; // required, not nullable
//	}
func presenceFixtureProto(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	field := func(name string, num int32, label descriptorpb.FieldDescriptorProto_Label,
		typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(num), Label: label.Enum(), Type: typ.Enum(),
		}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	oneof := func(f *descriptorpb.FieldDescriptorProto, idx int32) *descriptorpb.FieldDescriptorProto {
		f.OneofIndex = proto.Int32(idx)
		return f
	}
	optInt := oneof(field("opt", 10, opt, descriptorpb.FieldDescriptorProto_TYPE_INT32, ""), 1)
	optInt.Proto3Optional = proto.Bool(true)
	required := func(f *descriptorpb.FieldDescriptorProto) *descriptorpb.FieldDescriptorProto {
		f.Options = &descriptorpb.FieldOptions{}
		proto.SetExtension(f.Options, annotations.E_FieldBehavior, []annotations.FieldBehavior{annotations.FieldBehavior_REQUIRED})
		return f
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("fixture/v1/fixture.proto"),
		Package:    proto.String("fixture.v1"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto", "google/protobuf/wrappers.proto"},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name:  proto.String("Kind"),
			Value: []*descriptorpb.EnumValueDescriptorProto{{Name: proto.String("KIND_UNSPECIFIED"), Number: proto.Int32(0)}},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Other")},
			{
				Name: proto.String("Thing"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("name", 1, opt, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
					field("kind", 2, opt, descriptorpb.FieldDescriptorProto_TYPE_ENUM, ".fixture.v1.Kind"),
					field("seen_at", 3, opt, msg, ".google.protobuf.Timestamp"),
					field("count", 4, opt, msg, ".google.protobuf.Int32Value"),
					field("other", 5, opt, msg, ".fixture.v1.Other"),
					field("others", 6, rep, msg, ".fixture.v1.Other"),
					field("tally", 7, rep, msg, ".fixture.v1.Thing.TallyEntry"),
					oneof(field("a", 8, opt, msg, ".fixture.v1.Other"), 0),
					oneof(field("b", 9, opt, msg, ".fixture.v1.Other"), 0),
					optInt,
					required(field("must", 11, opt, msg, ".fixture.v1.Other")),
					required(field("made_at", 12, opt, msg, ".google.protobuf.Timestamp")),
				},
				NestedType: []*descriptorpb.DescriptorProto{{
					Name: proto.String("TallyEntry"),
					Field: []*descriptorpb.FieldDescriptorProto{
						field("key", 1, opt, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
						field("value", 2, opt, descriptorpb.FieldDescriptorProto_TYPE_INT32, ""),
					},
					Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
				}},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("detail")}, {Name: proto.String("_opt")}},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	require.NoError(t, err)
	return fd
}

func TestPresenceFields_FollowsProtojsonEmitUnpopulated(t *testing.T) {
	got := presenceFields(presenceFixtureProto(t))
	assert.Equal(t, map[string]presence{"v1Thing": {
		required: []string{"name", "kind", "seenAt", "count", "other", "others", "tally", "must", "madeAt"},
		nullable: []string{"seenAt", "count", "other"},
	}}, got)
}

// nullableSwagger is what protoc-gen-openapiv2 emits for the fixture: the
// wrapper is a plain integer and the message fields are $refs, two with
// descriptions beside the $ref.
const nullableSwagger = `{
  "swagger": "2.0",
  "info": {"title": "The Grid API", "version": "v1"},
  "paths": {
    "/api/v1/things/{name}": {
      "get": {
        "operationId": "GetThing",
        "parameters": [{"name": "name", "in": "path", "required": true, "type": "string"}],
        "responses": {"200": {"description": "ok", "schema": {"$ref": "#/definitions/v1Thing"}}}
      }
    }
  },
  "definitions": {
    "v1Kind": {"type": "string", "enum": ["KIND_UNSPECIFIED"], "default": "KIND_UNSPECIFIED"},
    "v1Other": {"type": "object"},
    "v1Thing": {"type": "object", "properties": {
      "name": {"type": "string"},
      "kind": {"$ref": "#/definitions/v1Kind", "description": "The kind."},
      "seenAt": {"type": "string", "format": "date-time"},
      "count": {"type": "integer", "format": "int32", "description": "How many."},
      "other": {"$ref": "#/definitions/v1Other"},
      "others": {"type": "array", "items": {"type": "object", "$ref": "#/definitions/v1Other"}},
      "tally": {"type": "object", "additionalProperties": {"type": "integer", "format": "int32"}},
      "a": {"$ref": "#/definitions/v1Other", "description": "Detail A."},
      "b": {"$ref": "#/definitions/v1Other"},
      "opt": {"type": "integer", "format": "int32"},
      "must": {"$ref": "#/definitions/v1Other"},
      "madeAt": {"type": "string", "format": "date-time", "description": "When it was made."}
    }, "required": ["must", "madeAt"]}
  }
}`

func TestBuild_MarksPresence(t *testing.T) {
	out, _, err := Build([]byte(nullableSwagger), nil, presenceFixtureProto(t))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	props := at(t, doc, "components", "schemas", "v1Thing", "properties").(map[string]any)
	prop := func(name string) map[string]any { return props[name].(map[string]any) }

	// Every field but a oneof member always has a key.
	assert.Equal(t,
		[]any{"count", "kind", "madeAt", "must", "name", "other", "others", "seenAt", "tally"},
		at(t, doc, "components", "schemas", "v1Thing", "required"))
	// Scalars, enums, lists, maps, oneof members and REQUIRED fields are never null.
	for _, name := range []string{"name", "kind", "others", "tally", "a", "b", "opt", "must", "madeAt"} {
		assert.NotContains(t, prop(name), "nullable", name)
	}
	// Timestamps and wrappers keep their scalar type and gain nullable.
	assert.Equal(t, map[string]any{"type": "string", "format": "date-time", "nullable": true}, prop("seenAt"))
	assert.Equal(t, true, prop("count")["nullable"])
	assert.Equal(t, "integer", prop("count")["type"])
	// A nullable message is allOf-wrapped: siblings of a bare $ref are ignored in 3.0.
	// It states the referenced type too: 3.0.3 ignores `nullable` without one.
	assert.Equal(t, map[string]any{
		"allOf":    []any{map[string]any{"$ref": "#/components/schemas/v1Other"}},
		"type":     "object",
		"nullable": true,
	}, prop("other"))
	// A $ref with a description keeps it through the 2.0 -> 3.0 conversion.
	assert.Equal(t, map[string]any{
		"allOf":       []any{map[string]any{"$ref": "#/components/schemas/v1Kind"}},
		"description": "The kind.",
	}, prop("kind"))
	assert.Equal(t, "Detail A.", prop("a")["description"])
	// A bare $ref with nothing to keep stays a bare $ref.
	assert.Equal(t, map[string]any{"$ref": "#/components/schemas/v1Other"}, prop("b"))
	// A REQUIRED message is not wrapped for nullability: still a bare $ref.
	assert.Equal(t, map[string]any{"$ref": "#/components/schemas/v1Other"}, prop("must"))
	assert.Equal(t, map[string]any{"type": "string", "format": "date-time", "description": "When it was made."}, prop("madeAt"))
}

func TestBuild_PresenceNamingDriftFails(t *testing.T) {
	// The descriptor says Thing has a `name`; a generator that named the
	// property differently must fail the build, not drop the markers.
	drifted := []byte(`{
  "swagger": "2.0", "info": {"title": "t", "version": "v1"}, "paths": {},
  "definitions": {"v1Thing": {"type": "object", "properties": {"seenAt": {"type": "string", "format": "date-time"}}}}
}`)
	_, _, err := Build(drifted, nil, presenceFixtureProto(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `schema v1Thing has no property "name"`)
}

// The served spec marks the fields clients most often get wrong: the
// evacuation count (null = unknown, never 0), source timestamps, and the
// no-footprint event geometry; leaves scalars alone; and types the fields the
// server always fills (REQUIRED in grid.proto) as present and non-null.
func TestCommittedSpecPresence(t *testing.T) {
	out, err := os.ReadFile("../../api/grid/v1/openapi.json")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	schema := func(name, prop string) map[string]any {
		return at(t, doc, "components", "schemas", name, "properties", prop).(map[string]any)
	}
	for _, c := range [][2]string{
		{"v1SummaryStats", "activeEvacuations"},
		{"v1Source", "lastSuccessAt"},
		{"v1Source", "lastAttemptAt"},
		{"v1Event", "geometry"},
		{"v1Event", "expires"},
		{"v1MeshAdminTelemetry", "batteryVolts"},
	} {
		assert.Equal(t, true, schema(c[0], c[1])["nullable"], c[0]+"."+c[1])
	}
	for _, c := range [][2]string{
		{"v1SummaryStats", "totalActive"},
		{"v1Source", "kind"},
		{"v1Event", "canonicalUrl"},
		{"v1Event", "wildfire"}, // oneof member: absent, not null
		{"v1Event", "provenance"},
		{"v1Event", "ingestedAt"},
		{"v1EventRevision", "event"},
		{"v1PlaceSummary", "summary"},
	} {
		assert.NotContains(t, schema(c[0], c[1]), "nullable", c[0]+"."+c[1])
	}
	required := func(name string) []any {
		return at(t, doc, "components", "schemas", name, "required").([]any)
	}
	assert.Contains(t, required("v1Event"), "provenance")
	assert.Contains(t, required("v1Event"), "geometry", "nullable, but always sent")
	assert.NotContains(t, required("v1Event"), "wildfire", "a oneof member is left out when unset")
	assert.Contains(t, required("v1SummaryStats"), "totalActive")
}

func TestCheckWire(t *testing.T) {
	md := presenceFixtureProto(t).Messages().ByName("Thing")
	full := `{"name":"","kind":"KIND_UNSPECIFIED","seenAt":null,"count":null,"other":null,
		"others":[{}],"tally":{},"must":{},"madeAt":"2026-11-07T00:00:00Z","a":{}}`
	seen := map[string]bool{}
	problems, err := CheckWire(md, []byte(full), seen)
	require.NoError(t, err)
	assert.Empty(t, problems)
	assert.Equal(t, map[string]bool{"fixture.v1.Thing.must": true, "fixture.v1.Thing.made_at": true}, seen)

	problems, err = CheckWire(md, []byte(`{"name":"","kind":"KIND_UNSPECIFIED","seenAt":null,"count":null,
		"others":[],"tally":{},"must":null,"madeAt":null}`), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"Thing.other: missing",
		"Thing.must: null, but the spec says it never is",
		"Thing.madeAt: null, but the spec says it never is",
	}, problems)
}

func TestRequiredFieldsReachable(t *testing.T) {
	md := presenceFixtureProto(t).Messages().ByName("Thing")
	assert.Equal(t, []string{"fixture.v1.Thing.made_at", "fixture.v1.Thing.must"}, RequiredFieldsReachable(md))
}
