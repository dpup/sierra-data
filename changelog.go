// Package sierradata holds files embedded from the repository root. Only
// CHANGELOG.md lives here: go:embed cannot reach above a package's directory,
// and the server publishes the changelog at /changelog.
package sierradata

import _ "embed"

// Changelog is CHANGELOG.md, rendered by cmd/server at /changelog.
//
//go:embed CHANGELOG.md
var Changelog []byte
