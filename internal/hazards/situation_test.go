package hazards

import "testing"

// TestLayerRegistryUnique guards the single source of truth: the registry that
// feeds both the dispatch map and the situation order must have no duplicate
// names and a non-nil builder per entry.
func TestLayerRegistryUnique(t *testing.T) {
	s := &Service{}
	seen := map[string]bool{}
	for _, e := range s.layerRegistry() {
		if e.name == "" || e.build == nil {
			t.Errorf("registry entry %+v has empty name or nil builder", e)
		}
		if seen[e.name] {
			t.Errorf("duplicate layer in registry: %q", e.name)
		}
		seen[e.name] = true
	}
}

func feat(sev string, headline string) Feature {
	p := Properties{Headline: headline, Source: Source{Name: "Test"}}
	p.setSeverity(sev)
	return Feature{Type: "Feature", Properties: p}
}
