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
}

// StructuredLocation represents both descriptive and coordinate location data
type StructuredLocation struct {
	Description string  `json:"description"` // Human-readable location (e.g., "Highway 4 eastbound near Angels Camp")
	Latitude    float64 `json:"latitude"`    // Decimal degrees
	Longitude   float64 `json:"longitude"`   // Decimal degrees
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
}

// AlertEnhancer interface defines AI-powered alert description enhancement
type AlertEnhancer interface {
	// Enhance single alert with AI processing
	EnhanceAlert(ctx context.Context, raw RawAlert) (EnhancedAlert, error)

	// Health check for AI service
	HealthCheck(ctx context.Context) error
}

// NewAlertEnhancer is implemented in enhancer.go
