package cache

import (
	"testing"
	"time"
)

func TestGetFiltersStaleButGetWithMetadataDoesNot(t *testing.T) {
	c := NewCache()
	if err := c.Set("k", "v", time.Minute, "test"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Fresh: both accessors return it.
	var got string
	if found, _ := c.Get("k", &got); !found || got != "v" {
		t.Fatalf("fresh Get: found=%v got=%q", found, got)
	}

	// Stale (past TTL, within 2x): Get refuses, GetWithMetadata serves.
	c.Backdate("k", 90*time.Second)
	if c.IsStale("k") != true {
		t.Fatal("expected entry to be stale after backdating past TTL")
	}
	if c.IsVeryStale("k") {
		t.Fatal("entry should not be very stale yet (within 2x TTL)")
	}
	got = ""
	if found, _ := c.Get("k", &got); found {
		t.Error("Get should filter stale entries")
	}
	got = ""
	entry, found, err := c.GetWithMetadata("k", &got)
	if err != nil || !found || got != "v" {
		t.Fatalf("stale GetWithMetadata: found=%v got=%q err=%v", found, got, err)
	}
	if entry == nil || entry.CreatedAt.IsZero() {
		t.Error("GetWithMetadata should return entry metadata")
	}

	// Very stale (past 2x TTL).
	c.Backdate("k", time.Minute)
	if !c.IsVeryStale("k") {
		t.Fatal("expected entry to be very stale after backdating past 2x TTL")
	}
}

func TestCleanupStaleKeepsServableStaleEntries(t *testing.T) {
	c := NewCache()
	_ = c.Set("fresh", 1, time.Minute, "test")
	_ = c.Set("stale", 2, time.Minute, "test")
	_ = c.Set("verystale", 3, time.Minute, "test")
	c.Backdate("stale", 90*time.Second)    // past TTL, within 2x
	c.Backdate("verystale", 3*time.Minute) // past 2x TTL

	removed := c.CleanupStale()
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (only the very-stale entry)", removed)
	}

	// The merely-stale entry must survive: services serve it as a fallback
	// when an upstream refresh fails.
	var v int
	if _, found, _ := c.GetWithMetadata("stale", &v); !found || v != 2 {
		t.Error("merely-stale entry should survive cleanup (needed for stale fallback)")
	}
	if _, found, _ := c.GetWithMetadata("verystale", &v); found {
		t.Error("very-stale entry should have been evicted")
	}
	var f int
	if found, _ := c.Get("fresh", &f); !found || f != 1 {
		t.Error("fresh entry should survive cleanup")
	}
}
