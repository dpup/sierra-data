package google

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	api "github.com/dpup/sierra-data/api/v1"
)

// MockHTTPDoer is a mock implementation of HTTPDoer
type MockHTTPDoer struct {
	mock.Mock
}

func (m *MockHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	args := m.Called(req)
	return args.Get(0).(*http.Response), args.Error(1)
}

// Helper function to load test fixture data
func loadTestFixture(t *testing.T, filename string) string {
	data, err := os.ReadFile("../../../tests/testdata/google/" + filename)
	require.NoError(t, err, "Failed to load test fixture %s", filename)
	return string(data)
}

// Helper function to create mock HTTP response
func createMockResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestComputeRoutes_Success(t *testing.T) {
	// Load test fixture with route data
	fixtureData := loadTestFixture(t, "seattle_portland.json")

	// Create mock HTTP client
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(200, fixtureData), nil)

	// Create client with mock
	client := NewClientWithHTTPDoer("test-api-key", "https://routes.googleapis.com", mockHTTP)

	// Test coordinates (Seattle to Portland)
	origin := &api.Coordinates{
		Latitude:  47.6062,
		Longitude: -122.3321,
	}
	destination := &api.Coordinates{
		Latitude:  45.5152,
		Longitude: -122.6784,
	}

	// Call ComputeRoutes
	routeData, err := client.ComputeRoutes(context.Background(), origin, destination)

	// Verify results
	require.NoError(t, err)
	require.NotNil(t, routeData)

	// Verify parsed route data from fixture
	assert.Equal(t, int32(9857), routeData.DurationSeconds, "Duration should match fixture")
	assert.Equal(t, int32(9859), routeData.StaticDurationSeconds, "Static duration should match fixture")
	assert.Equal(t, int32(280226), routeData.DistanceMeters, "Distance should match fixture")
	assert.NotEmpty(t, routeData.Polyline, "Polyline should be populated")

	// Verify mock was called
	mockHTTP.AssertExpectations(t)
}

func TestComputeRoutes_NoRoutes(t *testing.T) {
	// Create response with no routes
	emptyResponse := `{"routes": []}`

	// Create mock HTTP client
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(200, emptyResponse), nil)

	// Create client with mock
	client := NewClientWithHTTPDoer("test-api-key", "https://routes.googleapis.com", mockHTTP)

	origin := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}
	destination := &api.Coordinates{Latitude: 45.5152, Longitude: -122.6784}

	// Call ComputeRoutes
	routeData, err := client.ComputeRoutes(context.Background(), origin, destination)

	// Verify error handling
	assert.Error(t, err)
	assert.Nil(t, routeData)
	assert.Contains(t, err.Error(), "no routes found in response")

	mockHTTP.AssertExpectations(t)
}

func TestComputeRoutes_RateLimitError(t *testing.T) {
	// Create mock HTTP client that returns 429
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(429, `{"error": {"message": "Quota exceeded"}}`), nil)

	client := NewClientWithHTTPDoer("test-api-key", "https://routes.googleapis.com", mockHTTP)

	origin := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}
	destination := &api.Coordinates{Latitude: 45.5152, Longitude: -122.6784}

	// Call ComputeRoutes
	routeData, err := client.ComputeRoutes(context.Background(), origin, destination)

	// Verify error handling
	assert.Error(t, err)
	assert.Nil(t, routeData)
	assert.Contains(t, err.Error(), "rate limit exceeded")

	mockHTTP.AssertExpectations(t)
}

func TestComputeRoutes_APIError(t *testing.T) {
	// Create mock HTTP client that returns 400 (bad request)
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(400, `{"error": {"message": "Invalid coordinates"}}`), nil)

	client := NewClientWithHTTPDoer("test-api-key", "https://routes.googleapis.com", mockHTTP)

	origin := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}
	destination := &api.Coordinates{Latitude: 45.5152, Longitude: -122.6784}

	// Call ComputeRoutes
	routeData, err := client.ComputeRoutes(context.Background(), origin, destination)

	// Verify error handling
	assert.Error(t, err)
	assert.Nil(t, routeData)
	assert.Contains(t, err.Error(), "API error 400")

	mockHTTP.AssertExpectations(t)
}

func TestComputeRoutes_RequestFormat(t *testing.T) {
	// Create mock HTTP client that captures the request
	var capturedRequest *http.Request
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Run(func(args mock.Arguments) {
		capturedRequest = args.Get(0).(*http.Request)
	}).Return(createMockResponse(200, loadTestFixture(t, "seattle_portland.json")), nil)

	client := NewClientWithHTTPDoer("test-api-key", "https://routes.googleapis.com", mockHTTP)

	// Use specific coordinates to test formatting
	origin := &api.Coordinates{
		Latitude:  47.606209,
		Longitude: -122.332100,
	}
	destination := &api.Coordinates{
		Latitude:  45.515200,
		Longitude: -122.678400,
	}

	// Call ComputeRoutes
	_, err := client.ComputeRoutes(context.Background(), origin, destination)
	require.NoError(t, err)

	// Verify request was properly formatted
	require.NotNil(t, capturedRequest)
	assert.Equal(t, "POST", capturedRequest.Method)
	assert.Equal(t, "/directions/v2:computeRoutes", capturedRequest.URL.Path)

	// Verify required headers
	assert.Equal(t, "test-api-key", capturedRequest.Header.Get("X-Goog-Api-Key"))
	assert.Equal(t, "application/json", capturedRequest.Header.Get("Content-Type"))

	// Verify field mask header (critical for Google Routes API). Must NOT include
	// travelAdvisory.speedReadingIntervals - that would bill at the Enterprise SKU.
	expectedFieldMask := "routes.duration,routes.staticDuration,routes.distanceMeters,routes.polyline.encodedPolyline"
	assert.Equal(t, expectedFieldMask, capturedRequest.Header.Get("X-Goog-FieldMask"))

	// Verify request body contains expected coordinate structure
	body, err := io.ReadAll(capturedRequest.Body)
	require.NoError(t, err)
	bodyStr := string(body)

	// Check for coordinate precision (JSON marshaling may truncate trailing zeros)
	assert.Contains(t, bodyStr, "47.606209")
	assert.Contains(t, bodyStr, "-122.3321") // JSON truncates trailing zeros
	assert.Contains(t, bodyStr, "45.5152")   // JSON truncates trailing zeros
	assert.Contains(t, bodyStr, "-122.6784") // JSON truncates trailing zeros

	// Check for required request structure
	assert.Contains(t, bodyStr, "\"travelMode\":\"DRIVE\"")
	assert.Contains(t, bodyStr, "\"routingPreference\":\"TRAFFIC_AWARE_OPTIMAL\"")
	// Must NOT request traffic-on-polyline (Enterprise SKU).
	assert.NotContains(t, bodyStr, "TRAFFIC_ON_POLYLINE")

	mockHTTP.AssertExpectations(t)
}

func TestComputeRoutes_DurationParsing(t *testing.T) {
	// Test duration parsing with various formats
	testRouteResponse := `{
		"routes": [{
			"duration": "450s",
			"staticDuration": "400s",
			"distanceMeters": 50000,
			"polyline": {"encodedPolyline": "test_polyline"}
		}]
	}`

	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(200, testRouteResponse), nil)

	client := NewClientWithHTTPDoer("test-api-key", "https://routes.googleapis.com", mockHTTP)

	origin := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}
	destination := &api.Coordinates{Latitude: 45.5152, Longitude: -122.6784}

	// Call ComputeRoutes
	routeData, err := client.ComputeRoutes(context.Background(), origin, destination)

	// Verify duration parsing
	require.NoError(t, err)
	assert.Equal(t, int32(450), routeData.DurationSeconds)
	assert.Equal(t, int32(400), routeData.StaticDurationSeconds)
	assert.Equal(t, int32(50000), routeData.DistanceMeters)
	assert.Equal(t, "test_polyline", routeData.Polyline)

	mockHTTP.AssertExpectations(t)
}

func TestComputeRoutes_InvalidJSON(t *testing.T) {
	// Create mock HTTP client that returns invalid JSON
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(200, `{"invalid": json}`), nil)

	client := NewClientWithHTTPDoer("test-api-key", "https://routes.googleapis.com", mockHTTP)

	origin := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}
	destination := &api.Coordinates{Latitude: 45.5152, Longitude: -122.6784}

	// Call ComputeRoutes
	routeData, err := client.ComputeRoutes(context.Background(), origin, destination)

	// Verify error handling
	assert.Error(t, err)
	assert.Nil(t, routeData)
	assert.Contains(t, err.Error(), "failed to decode response")

	mockHTTP.AssertExpectations(t)
}
