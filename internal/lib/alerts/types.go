package alerts

import (
	"context"
	"time"
)

// RawAlert represents unprocessed alert data from Caltrans
type RawAlert struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"` // Incident title (e.g., "Northbound 101 Lane Closure")
	Description string    `json:"description"`
	Location    string    `json:"location"`
	StyleUrl    string    `json:"style_url,omitempty"` // KML style indicating closure type
	Timestamp   time.Time `json:"timestamp"`
	// PlaceNames grounds the model's geography: the ONLY localities it may name
	// beyond those in the text, closest first. The user prompt marshals this
	// struct, so the list reaches the model as `place_names` with no extra
	// plumbing, and the system prompt's "Place Names" rule binds it.
	//
	// EMPTY IS MEANINGFUL — it means we looked and nothing is near enough to
	// name, and the rule then forbids naming any locality at all. It is
	// therefore serialized EXPLICITLY as [] rather than omitted: an absent key
	// is indistinguishable from a caller that forgot to populate it, and the
	// explicit empty array is the stronger instruction. That is the case this
	// exists for: an incident at Sonora Pass, 52.8 km from the nearest
	// configured town, previously acquired "(near Merced)" — 134 km away, taken
	// from a CHP dispatch-centre token in the raw text.
	//
	// Deliberately NOT part of the content hash (HashRawAlert), because the list
	// is a deterministic function of coordinates already inside Location. One
	// consequence: adding a town to config does not invalidate cached
	// enhancements, so a stale list can be served for up to the 24h TTL.
	PlaceNames []string `json:"place_names"`
}

// StructuredLocation represents both descriptive and coordinate location data
type StructuredLocation struct {
	// Description is the human-readable location. It may name a locality ONLY
	// from RawAlert.PlaceNames or the incident text — see the "Place Names" rule
	// in SystemPrompt.
	Description string  `json:"description"`
	Latitude    float64 `json:"latitude"`  // Decimal degrees
	Longitude   float64 `json:"longitude"` // Decimal degrees
}

// StructuredDescription represents AI-processed alert information in standardized format
type StructuredDescription struct {
	TimeReported       string             `json:"time_reported,omitempty"`
	Details            string             `json:"details"`
	Location           StructuredLocation `json:"location"`
	LastUpdate         string             `json:"last_update,omitempty"`
	Impact             string             `json:"impact"`              // enum: none, light, moderate, severe
	RoadStatus         string             `json:"road_status"`         // enum: open, restricted, closed
	RestrictionDetails string             `json:"restriction_details"` // Details when restricted/closed
	ChainStatus        string             `json:"chain_status"`        // enum: none, r1, r2, active_unspecified
	AdditionalInfo     map[string]string  `json:"additional_info,omitempty"`
	CondensedSummary   string             `json:"condensed_summary,omitempty"`
}

// EnhancedAlert represents a fully processed alert with AI enhancement
type EnhancedAlert struct {
	ID                    string                `json:"id"`
	OriginalDescription   string                `json:"original_description"`
	StructuredDescription StructuredDescription `json:"structured_description"`
	CondensedSummary      string                `json:"condensed_summary"`
	ProcessedAt           time.Time             `json:"processed_at"`
	// Request/Response capture the model I/O for transparency (shown by clients).
	// Request is the incident-specific user prompt; Response is the raw
	// structured JSON returned. Cached alongside the rest so cache hits keep them.
	Request  string `json:"request,omitempty"`
	Response string `json:"response,omitempty"`
}

// AlertEnhancer interface defines AI-powered alert description enhancement
type AlertEnhancer interface {
	// Enhance single alert with AI processing
	EnhanceAlert(ctx context.Context, raw RawAlert) (EnhancedAlert, error)

	// Health check for AI service
	HealthCheck(ctx context.Context) error
}

// NewAlertEnhancer is implemented in enhancer.go
