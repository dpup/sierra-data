package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"
)

// ValidateEnvOverrides reports PF__ environment variables that target this
// service's own config namespaces but map to no real key, so they are silently
// ignored.
//
// WHY THIS EXISTS. Prefab validates unknown config keys, but `cmd/server`
// deliberately registers our namespaces (`grid`, `roads`, `weather`, …) as
// opaque objects to stop it warning about every one of our keys. That silences
// the check for everything INSIDE them — so a misnamed override just does
// nothing, with no warning, and the config file's value quietly stays.
//
// That is not a cosmetic failure. `PF__GRID__DBPATH` (the wrong spelling of
// `PF__GRID__DB_PATH`) left the event store at prefab.yaml's relative
// `./data/grid.db` instead of the mounted volume — which on a container means
// the entire revision history is discarded on every replacement, and in the dev
// sandbox put the DB on the virtiofs bind mount that corrupts SQLite. Both
// present as "it works", right up until the data is gone.
//
// THE TRAP. Prefab maps `PF__A__B_C` -> `a.bC`: `__` separates path segments and
// an `_` INSIDE a segment produces the next capital. So a camelCase key needs
// the underscore — `grid.dbPath` is `PF__GRID__DB_PATH`. Drop it and
// `PF__GRID__DBPATH` becomes `grid.dbpath`, which matches nothing.
//
// Keys are resolved by reflecting over Config, so this stays correct as fields
// are added — nobody has to remember to register a new one. Env vars outside our
// top-level namespaces are ignored here: prefab still owns `server.*`.
func ValidateEnvOverrides(environ []string) error {
	cfgType := reflect.TypeOf(Config{})
	var problems []string

	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "PF__") {
			continue
		}
		key := envToConfigKey(name)
		segs := strings.Split(key, ".")
		if !ownsNamespace(cfgType, segs[0]) {
			continue // prefab's own (server.*, …) — not ours to police
		}
		if _, ok := resolveConfigPath(cfgType, segs, false); ok {
			continue
		}
		msg := fmt.Sprintf("%s (resolves to config key %q, which does not exist)", name, key)
		if canonical, ok := resolveConfigPath(cfgType, segs, true); ok {
			msg += fmt.Sprintf(" — did you mean %s, for key %q?",
				configKeyToEnv(canonical), strings.Join(canonical, "."))
		}
		problems = append(problems, msg)
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("silently-ignored environment override(s):\n  - %s\n"+
		"A camelCase config key needs an underscore in its env var "+
		"(PF__A__B_C -> a.bC), e.g. grid.dbPath is PF__GRID__DB_PATH",
		strings.Join(problems, "\n  - "))
}

// ownsNamespace reports whether seg names one of Config's top-level sections.
// Matched case-insensitively so a correctly-spelled namespace with a broken leaf
// is still checked.
func ownsNamespace(cfgType reflect.Type, seg string) bool {
	for i := 0; i < cfgType.NumField(); i++ {
		if tag := koanfTag(cfgType.Field(i)); tag != "" && strings.EqualFold(tag, seg) {
			return true
		}
	}
	return false
}

// resolveConfigPath walks the dotted segments through t and returns the
// canonical path. With fold=false segment names must match a koanf tag exactly;
// with fold=true they match case-insensitively, which is what turns a failure
// into a "did you mean" (the whole bug class is a casing mismatch).
//
// Map fields consume one segment as the map key and keep validating the element
// type, so grid.sources.usgs.pollInterval is checked down to the leaf. Slices
// are accepted wholesale: koanf cannot merge an env key onto an array element
// anyway, so there is nothing meaningful to validate and flagging would only
// produce false positives.
func resolveConfigPath(t reflect.Type, segs []string, fold bool) ([]string, bool) {
	if len(segs) == 0 {
		return nil, true
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			tag := koanfTag(t.Field(i))
			if tag == "" {
				continue
			}
			if tag == segs[0] || (fold && strings.EqualFold(tag, segs[0])) {
				rest, ok := resolveConfigPath(t.Field(i).Type, segs[1:], fold)
				if !ok {
					return nil, false
				}
				return append([]string{tag}, rest...), true
			}
		}
		return nil, false

	case reflect.Map:
		// segs[0] is an arbitrary map key (a source id); keep its spelling.
		rest, ok := resolveConfigPath(t.Elem(), segs[1:], fold)
		if !ok {
			return nil, false
		}
		return append([]string{segs[0]}, rest...), true

	case reflect.Slice, reflect.Array:
		return segs, true

	default:
		return nil, false // a scalar with path left over: too deep
	}
}

func koanfTag(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("koanf"), ",")
	return tag
}

// envToConfigKey mirrors prefab's TransformEnv. It lives in prefab's internal/
// package so it cannot be imported; TestEnvToConfigKey pins the examples from
// prefab's own doc comment so a drift shows up as a test failure.
func envToConfigKey(name string) string {
	s := strings.ToLower(strings.TrimPrefix(name, "PF__"))
	segments := strings.Split(s, "__")
	for i, segment := range segments {
		parts := strings.Split(segment, "_")
		for j := 1; j < len(parts); j++ {
			if parts[j] != "" {
				r := []rune(parts[j])
				r[0] = unicode.ToUpper(r[0])
				parts[j] = string(r)
			}
		}
		segments[i] = strings.Join(parts, "")
	}
	return strings.Join(segments, ".")
}

// configKeyToEnv is the inverse: the env var that actually sets this key.
// camelCase becomes UPPER_SNAKE within a segment; segments join with "__".
func configKeyToEnv(segs []string) string {
	out := make([]string, len(segs))
	for i, seg := range segs {
		var b strings.Builder
		for _, r := range seg {
			if unicode.IsUpper(r) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToUpper(r))
		}
		out[i] = b.String()
	}
	return "PF__" + strings.Join(out, "__")
}
