package hazards

import (
	"context"
	"errors"
	"testing"

	"github.com/dpup/prefab/logging"

	"github.com/dpup/sierra-data/internal/cache"
	"github.com/dpup/sierra-data/internal/config"
)

// testCtx carries a logger so buildLayer's logging.Errorw/Warnw don't panic.
func testCtx() context.Context { return logging.EnsureLogger(context.Background()) }

func okBuild(fs ...Feature) builder {
	return func(context.Context, config.HazardArea) ([]Feature, error) { return fs, nil }
}
func errBuild(err error) builder {
	return func(context.Context, config.HazardArea) ([]Feature, error) { return nil, err }
}

// TestBuildLayer_FreshCacheHit: a second request inside the TTL is served from
// cache without invoking the builder again.
func TestBuildLayer_FreshCacheHit(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	area := config.HazardArea{ID: "x"}
	r1 := s.buildLayer(testCtx(), area, LayerEarthquake, okBuild(feat(SevMinor, "q")))
	if r1.status != "OK" || len(r1.features) != 1 {
		t.Fatalf("first build = %q/%d", r1.status, len(r1.features))
	}
	// Builder now fails the test if called — the fresh cache must satisfy this.
	poison := func(context.Context, config.HazardArea) ([]Feature, error) {
		t.Error("builder should not be called on a fresh cache hit")
		return nil, nil
	}
	r2 := s.buildLayer(testCtx(), area, LayerEarthquake, poison)
	if r2.status != "OK" || len(r2.features) != 1 {
		t.Fatalf("cached build = %q/%d", r2.status, len(r2.features))
	}
}

// TestBuildLayer_StaleOnError: when the upstream fails but a (stale) cached value
// exists, the layer degrades to STALE serving last-good data — not UNAVAILABLE.
func TestBuildLayer_StaleOnError(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	area := config.HazardArea{ID: "x"}
	key := "hazard:x:" + LayerEarthquake
	// Inject an already-stale entry (TTL 0 => ExpiresAt == now).
	if err := s.cache.Set(key, []Feature{feat(SevModerate, "old")}, 0, "test"); err != nil {
		t.Fatal(err)
	}
	r := s.buildLayer(testCtx(), area, LayerEarthquake, errBuild(errors.New("boom")))
	if r.status != "STALE" {
		t.Fatalf("status = %q, want STALE", r.status)
	}
	if len(r.features) != 1 {
		t.Fatalf("stale features = %d, want 1 (last good)", len(r.features))
	}
	if r.lastSourceUpdate.IsZero() {
		t.Error("STALE result must carry last_source_update")
	}
}

// TestBuildLayer_UnavailableNoCache: upstream fails with nothing cached => fail-loud.
func TestBuildLayer_UnavailableNoCache(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	r := s.buildLayer(testCtx(), config.HazardArea{ID: "x"}, LayerEarthquake, errBuild(errors.New("boom")))
	if r.status != "UNAVAILABLE" || len(r.features) != 0 {
		t.Fatalf("got %q/%d, want UNAVAILABLE/0", r.status, len(r.features))
	}
}

// TestBuildLayer_PartialIsStale: a builder returning partialData keeps its
// (incomplete) features and reports STALE, not a silent OK.
func TestBuildLayer_PartialIsStale(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	partial := func(context.Context, config.HazardArea) ([]Feature, error) {
		return []Feature{feat(SevSevere, "one source")}, partialData(errors.New("other source down"))
	}
	r := s.buildLayer(testCtx(), config.HazardArea{ID: "x"}, LayerWildfire, partial)
	if r.status != "STALE" || len(r.features) != 1 {
		t.Fatalf("got %q/%d, want STALE/1", r.status, len(r.features))
	}
}

// TestBuildLayer_EvacEmptyIsOK: a clean empty Cal OES result is OK with zero
// features (confirmed "no active zones" — not UNAVAILABLE). It must still not be
// cached, so a later fetch error falls through to UNAVAILABLE rather than
// replaying a stale "0".
func TestBuildLayer_EvacEmptyIsOK(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	r := s.buildLayer(testCtx(), config.HazardArea{ID: "x"}, LayerEvacuation, okBuild())
	if r.status != "OK" {
		t.Fatalf("clean-empty evac status = %q, want OK", r.status)
	}
	if len(r.features) != 0 {
		t.Fatalf("clean-empty evac features = %d, want 0", len(r.features))
	}
	if r.meta.sourceURL == "" {
		t.Error("evac must carry the Genasys source URL even when empty")
	}
	var anything []Feature
	if ok, _ := s.cache.Get("hazard:x:"+LayerEvacuation, &anything); ok {
		t.Error("empty evac result must not be cached")
	}
}

// TestBuildLayer_EvacErrorUnavailable: a Cal OES error (NOT a clean empty) is
// UNAVAILABLE — the consumer-visible difference that lets "no zones" be told
// apart from "feed broken".
func TestBuildLayer_EvacErrorUnavailable(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	r := s.buildLayer(testCtx(), config.HazardArea{ID: "x"}, LayerEvacuation, errBuild(errors.New("cal oes down")))
	if r.status != "UNAVAILABLE" {
		t.Fatalf("evac error status = %q, want UNAVAILABLE", r.status)
	}
}
