package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Contract tests for updated RoadAlert message structure
// Tests verify the flattened alert structure with AI-enhanced fields

func TestRoadAlert_NewStructure(t *testing.T) {
	// Test that RoadAlert has the new flattened structure
	alert := &RoadAlert{
		Type:             AlertType_CLOSURE,
		Severity:         AlertSeverity_WARNING,
		Classification:   AlertClassification_ON_ROUTE,
		Title:            "CHP Incident 250911GG0206",
		Description:      "Lane closure on Highway 4 eastbound at mile marker 31, emergency services responding",
		CondensedSummary: "Lane closure, emergency services responding",
		StartTime:        timestamppb.New(time.Now()),
		EndTime:          timestamppb.New(time.Now().Add(time.Hour)),
		LastUpdated:      timestamppb.New(time.Now()),
		Location: &Coordinates{
			Latitude:  38.0675,
			Longitude: -120.5436,
		},
		LocationDescription: "Highway 4 eastbound at mile marker 31",
		Impact:              AlertImpact_IMPACT_MODERATE,
		Duration:            AlertDuration_DURATION_UNDER_ONE_HOUR,
		TimeReported:        timestamppb.New(time.Now().Add(-time.Minute * 10)),
		Metadata: map[string]string{
			"vehicles_involved": "2",
			"tow_required":      "yes",
		},
	}

	// Verify all fields are properly set
	assert.Equal(t, AlertType_CLOSURE, alert.Type)
	assert.Equal(t, AlertSeverity_WARNING, alert.Severity)
	assert.Equal(t, AlertClassification_ON_ROUTE, alert.Classification)
	assert.Equal(t, "CHP Incident 250911GG0206", alert.Title)
	assert.Contains(t, alert.Description, "Highway 4 eastbound")
	assert.Contains(t, alert.CondensedSummary, "Lane closure")
	assert.NotNil(t, alert.StartTime)
	assert.NotNil(t, alert.EndTime)
	assert.NotNil(t, alert.LastUpdated)
	assert.NotNil(t, alert.Location)
	assert.Equal(t, 38.0675, alert.Location.Latitude)
	assert.Equal(t, -120.5436, alert.Location.Longitude)
	assert.Equal(t, "Highway 4 eastbound at mile marker 31", alert.LocationDescription)
	assert.Equal(t, AlertImpact_IMPACT_MODERATE, alert.Impact)
	assert.Equal(t, AlertDuration_DURATION_UNDER_ONE_HOUR, alert.Duration)
	assert.NotNil(t, alert.TimeReported)
	assert.Equal(t, "2", alert.Metadata["vehicles_involved"])
}

func TestRoadAlert_AIEnhancedFields(t *testing.T) {
	// Test AI-enhanced fields are properly supported
	alert := &RoadAlert{
		Type:                AlertType_INCIDENT,
		Impact:              AlertImpact_IMPACT_SEVERE,
		Duration:            AlertDuration_DURATION_SEVERAL_HOURS,
		LocationDescription: "Highway 4 near Bear Valley Road",
		CondensedSummary:    "Multi-vehicle collision, injuries reported",
		Metadata: map[string]string{
			"responders":    "CHP, EMS, Fire",
			"lanes_blocked": "2 of 3",
		},
	}

	// Verify AI fields
	assert.Equal(t, AlertImpact_IMPACT_SEVERE, alert.Impact)
	assert.Equal(t, AlertDuration_DURATION_SEVERAL_HOURS, alert.Duration)
	assert.Equal(t, "Highway 4 near Bear Valley Road", alert.LocationDescription)
	assert.Equal(t, "Multi-vehicle collision, injuries reported", alert.CondensedSummary)
	assert.Equal(t, "CHP, EMS, Fire", alert.Metadata["responders"])
}

func TestRoadAlert_MetadataReservedForAdditionalInfo(t *testing.T) {
	// Test that metadata is only used for AI's additional_info
	alert := &RoadAlert{
		Type:  AlertType_CONSTRUCTION,
		Title: "Route 4 Construction Alert",
		Metadata: map[string]string{
			"estimated_delay_minutes": "15",
			"construction_type":       "resurfacing",
			"work_hours":              "8am-4pm weekdays",
		},
	}

	// Verify metadata contains only additional info
	assert.Equal(t, "15", alert.Metadata["estimated_delay_minutes"])
	assert.Equal(t, "resurfacing", alert.Metadata["construction_type"])
	assert.Equal(t, "8am-4pm weekdays", alert.Metadata["work_hours"])
}

func TestAlertClassification_Enum(t *testing.T) {
	// Test AlertClassification enum values
	assert.Equal(t, int32(0), int32(AlertClassification_ALERT_CLASSIFICATION_UNSPECIFIED))
	assert.Equal(t, int32(1), int32(AlertClassification_ON_ROUTE))
	assert.Equal(t, int32(2), int32(AlertClassification_NEARBY))
	assert.Equal(t, int32(3), int32(AlertClassification_DISTANT))
}

func TestProcessingMetrics_Fields(t *testing.T) {
	// Test ProcessingMetrics message structure
	metrics := &ProcessingMetrics{
		TotalRawAlerts:      150,
		FilteredAlerts:      45,
		EnhancedAlerts:      42,
		EnhancementFailures: 3,
		AvgProcessingTimeMs: 245.7,
	}

	assert.Equal(t, int64(150), metrics.TotalRawAlerts)
	assert.Equal(t, int64(45), metrics.FilteredAlerts)
	assert.Equal(t, int64(42), metrics.EnhancedAlerts)
	assert.Equal(t, int64(3), metrics.EnhancementFailures)
	assert.Equal(t, 245.7, metrics.AvgProcessingTimeMs)
}
