// Package placesdata embeds the checked-in place-directory seed geometry.
// go:embed cannot reference files outside the package directory, so this
// package lives beside the data it exposes; internal/places consumes it.
package placesdata

import _ "embed"

// CountiesGeoJSON is the Census TIGERweb generalized (1:500k) county polygon
// FeatureCollection. Provenance and post-processing: README.md alongside.
//
//go:embed counties.geojson
var CountiesGeoJSON []byte
