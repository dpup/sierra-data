package main

import (
	"cmp"
	"maps"
	"slices"
)

func sortedKeys[V any, K cmp.Ordered](m map[K]V) []K {
	return slices.Sorted(maps.Keys(m))
}
