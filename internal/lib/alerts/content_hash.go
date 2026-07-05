package alerts

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// ContentHasher provides simple content-based deduplication for alerts
type ContentHasher struct{}

// NewContentHasher creates a new content hasher
func NewContentHasher() *ContentHasher {
	return &ContentHasher{}
}

// HashRawAlert creates a content hash for deduplication
// Much simpler than the complex incident hashing system
func (h *ContentHasher) HashRawAlert(raw RawAlert) string {
	// Normalize the description text for consistent hashing
	normalizedDesc := h.normalizeText(raw.Description)
	normalizedTitle := h.normalizeText(raw.Title)

	// Create a content signature including title, description and location
	// This catches the same incident reported with minor text variations
	contentSignature := fmt.Sprintf("%s|%s|%s|%s",
		normalizedTitle,
		normalizedDesc,
		h.normalizeText(raw.Location),
		raw.StyleUrl, // Include StyleUrl as it indicates incident type
	)

	// Generate SHA-256 hash
	hash := sha256.Sum256([]byte(contentSignature))
	return fmt.Sprintf("%x", hash)
}

// normalizeText cleans text for consistent hashing
// Handles common variations in Caltrans incident descriptions
func (h *ContentHasher) normalizeText(text string) string {
	// Convert to lowercase
	normalized := strings.ToLower(text)

	// Remove extra whitespace
	normalized = regexp.MustCompile(`\s+`).ReplaceAllString(normalized, " ")

	// Remove time-specific elements that change while the content remains the
	// same — BEFORE punctuation stripping, which would mangle "12:55pm" into
	// "1255pm" and hide it from these patterns. This matters: CHP descriptions
	// embed a "Last updated: <time>" stamp and per-line dispatch timestamps
	// ("jul 4 2026 12:32pm") that tick on every feed poll — without stripping
	// them, every incident re-hashes as new content on every refresh and the
	// 24h enhancement cache never hits. Genuinely new detail lines still
	// change the hash (their non-timestamp text survives).
	normalized = regexp.MustCompile(`\d{1,2}/\d{1,2}/\d{4}`).ReplaceAllString(normalized, "")
	normalized = regexp.MustCompile(`\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)\s+\d{1,2}\s+\d{4}\b`).ReplaceAllString(normalized, "")
	normalized = regexp.MustCompile(`\b\d{1,2}:\d{2}\s*([ap]m)?\b`).ReplaceAllString(normalized, "")

	// Remove common punctuation that varies
	normalized = regexp.MustCompile(`[.,;:!?()-]`).ReplaceAllString(normalized, "")

	// Normalize common abbreviations
	replacements := map[string]string{
		"hwy":      "highway",
		"nb":       "northbound",
		"sb":       "southbound",
		"eb":       "eastbound",
		"wb":       "westbound",
		"incident": "inc",
		"closure":  "closed",
	}

	for abbrev, full := range replacements {
		normalized = strings.ReplaceAll(normalized, abbrev, full)
	}

	return strings.TrimSpace(normalized)
}
