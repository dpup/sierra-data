package hazards

import (
	"context"
	"errors"
	"testing"
	"time"

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

// TestBuildLayer_PartialIsCachedAsStale: a partial (STALE-with-data) result is
// cached for the layer's TTL, so a degraded upstream is fetched at most once per
// TTL instead of on every request (message_sign re-downloaded 125KB+ per request
// while one sign lacked a position). The cached entry must still read STALE —
// never promoted to OK — and a later clean fetch must replace it.
func TestBuildLayer_PartialIsCachedAsStale(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	area := config.HazardArea{ID: "x"}
	calls := 0
	partial := func(context.Context, config.HazardArea) ([]Feature, error) {
		calls++
		return []Feature{feat(SevInfo, "sign")}, partialData(errors.New("sign without a position"))
	}

	r1 := s.buildLayer(testCtx(), area, LayerMessageSign, partial)
	if r1.status != "STALE" || len(r1.features) != 1 {
		t.Fatalf("first build = %q/%d, want STALE/1", r1.status, len(r1.features))
	}
	r2 := s.buildLayer(testCtx(), area, LayerMessageSign, partial)
	if calls != 1 {
		t.Fatalf("builder called %d times inside the TTL, want 1", calls)
	}
	if r2.status != "STALE" || len(r2.features) != 1 {
		t.Fatalf("cached partial = %q/%d, want STALE/1 (must not be promoted to OK)", r2.status, len(r2.features))
	}
	if r2.lastSourceUpdate.IsZero() {
		t.Error("cached STALE result must carry last_source_update")
	}

	// TTL elapses; upstream heals. The clean fetch replaces the partial entry.
	s.cache.Backdate("hazard:x:"+LayerMessageSign, 10*time.Minute)
	r3 := s.buildLayer(testCtx(), area, LayerMessageSign, okBuild(feat(SevInfo, "a"), feat(SevInfo, "b")))
	if r3.status != "OK" || len(r3.features) != 2 {
		t.Fatalf("clean refetch = %q/%d, want OK/2", r3.status, len(r3.features))
	}
	r4 := s.buildLayer(testCtx(), area, LayerMessageSign, partial)
	if calls != 1 || r4.status != "OK" || len(r4.features) != 2 {
		t.Fatalf("after clean fetch got %q/%d (calls=%d), want cached OK/2", r4.status, len(r4.features), calls)
	}
}

// TestBuildLayer_ErrorAfterPartialServesStale: a hard error once a cached
// partial has expired still serves it as last-good STALE + lastSourceUpdate.
func TestBuildLayer_ErrorAfterPartialServesStale(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	area := config.HazardArea{ID: "x"}
	partial := func(context.Context, config.HazardArea) ([]Feature, error) {
		return []Feature{feat(SevInfo, "sign")}, partialData(errors.New("degraded"))
	}
	s.buildLayer(testCtx(), area, LayerMessageSign, partial)
	s.cache.Backdate("hazard:x:"+LayerMessageSign, 10*time.Minute)
	r := s.buildLayer(testCtx(), area, LayerMessageSign, errBuild(errors.New("down")))
	if r.status != "STALE" || len(r.features) != 1 || r.lastSourceUpdate.IsZero() {
		t.Fatalf("got %q/%d lastUpdate=%v, want STALE/1 with lastSourceUpdate", r.status, len(r.features), r.lastSourceUpdate)
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
	// Evacuation is deliberately excluded from empty-caching (cacheEmptyResults):
	// caching an empty would delay the FIRST evacuation order by up to the TTL,
	// which is the one transition where delay is least acceptable. Other layers
	// DO cache empties — see TestBuildLayer_EmptySuccessIsCached.
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

// TestBuildLayer_EmptySuccessIsCached: a clean empty result is now cached, so a
// layer that is legitimately empty most of the time stops re-fetching its
// upstream on every single request.
//
// chain_control is the motivating case: outside snow season it is empty by
// definition, and because empties were never cached it re-parsed the Caltrans
// KML on every /summary and every chain_control.geojson — measured 36-49 ms per
// request, indefinitely, for a 212-byte answer that never changed.
func TestBuildLayer_EmptySuccessIsCached(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	area := config.HazardArea{ID: "x"}

	r1 := s.buildLayer(testCtx(), area, LayerChainControl, okBuild())
	if r1.status != "OK" || len(r1.features) != 0 {
		t.Fatalf("clean empty must be OK+0, got %q/%d", r1.status, len(r1.features))
	}

	poison := func(context.Context, config.HazardArea) ([]Feature, error) {
		t.Error("builder must not be called again: the empty success should be cached")
		return nil, nil
	}
	r2 := s.buildLayer(testCtx(), area, LayerChainControl, poison)
	if r2.status != "OK" || len(r2.features) != 0 {
		t.Fatalf("cached empty must still read OK+0, got %q/%d", r2.status, len(r2.features))
	}
}

// TestBuildLayer_CachedEmptyNeverSurvivesAnError is the fail-loud counterpart,
// and the reason caching empties is safe at all.
//
// The invariant is "an error never becomes a 0". Now that a clean empty IS
// cached, the ONLY thing standing between an upstream failure and a fabricated
// all-clear is the len(stale) > 0 guard on buildLayer's error path. If that guard
// is ever dropped, this fails: the layer would answer a hard error with the
// cached empty and report OK/STALE + 0 features — "nothing to worry about" —
// when the true answer is "we do not know".
func TestBuildLayer_CachedEmptyNeverSurvivesAnError(t *testing.T) {
	s := &Service{cache: cache.NewCache()}
	area := config.HazardArea{ID: "x"}

	// Prime the cache with a clean empty success.
	if r := s.buildLayer(testCtx(), area, LayerChainControl, okBuild()); r.status != "OK" {
		t.Fatalf("priming build = %q", r.status)
	}
	// Expire it so the error path's stale-fallback is the branch under test.
	s.cache.Set("hazard:x:"+LayerChainControl, []Feature{}, 0, "hazard:"+LayerChainControl)

	r := s.buildLayer(testCtx(), area, LayerChainControl, errBuild(errors.New("upstream down")))
	if r.status != "UNAVAILABLE" {
		t.Fatalf("an upstream error over a cached EMPTY must be UNAVAILABLE, got %q — "+
			"a cached empty replayed after an error is a fabricated all-clear", r.status)
	}
	if len(r.features) != 0 {
		t.Fatalf("UNAVAILABLE must carry no features, got %d", len(r.features))
	}
}
