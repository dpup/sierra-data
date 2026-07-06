package weather

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
	data, err := os.ReadFile("../../../tests/testdata/weather/" + filename)
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

func TestGetWeatherAlerts_WithAlerts(t *testing.T) {
	// Load test fixture with alerts
	fixtureData := loadTestFixture(t, "seattle_alerts_test.json")

	// Create mock HTTP client
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(200, fixtureData), nil)

	// Create client with mock
	client := NewClientWithHTTPDoer("test-api-key", "https://api.openweathermap.org", mockHTTP)

	// Test coordinates
	coordinates := &api.Coordinates{
		Latitude:  47.6062,
		Longitude: -122.3321,
	}

	// Call GetWeatherAlerts
	alerts, err := client.GetWeatherAlerts(context.Background(), coordinates)

	// Verify results
	require.NoError(t, err)
	require.NotNil(t, alerts)
	assert.Len(t, alerts, 2, "Should have 2 alerts from test fixture")

	// Verify first alert (Wind Advisory)
	windAlert := alerts[0]
	assert.Equal(t, "NWS Seattle", windAlert.SenderName)
	assert.Equal(t, "Wind Advisory", windAlert.Event)
	assert.Equal(t, int64(1694880000), windAlert.GetStartTime().AsTime().Unix())
	assert.Equal(t, int64(1694966400), windAlert.GetEndTime().AsTime().Unix())
	assert.Contains(t, windAlert.Description, "Winds 25 to 35 mph")
	assert.Contains(t, windAlert.Tags, "high wind")
	assert.NotEmpty(t, windAlert.Id, "Alert ID should be generated")

	// Verify second alert (Flood Watch)
	floodAlert := alerts[1]
	assert.Equal(t, "NWS Seattle", floodAlert.SenderName)
	assert.Equal(t, "Flood Watch", floodAlert.Event)
	assert.Equal(t, int64(1694890000), floodAlert.GetStartTime().AsTime().Unix())
	assert.Equal(t, int64(1694976400), floodAlert.GetEndTime().AsTime().Unix())
	assert.Contains(t, floodAlert.Description, "Heavy rainfall")
	assert.Contains(t, floodAlert.Tags, "rain")
	assert.Contains(t, floodAlert.Tags, "flood")
	assert.NotEmpty(t, floodAlert.Id, "Alert ID should be generated")

	// Verify all alerts have unique IDs
	assert.NotEqual(t, windAlert.Id, floodAlert.Id, "Alert IDs should be unique")

	// Verify mock was called
	mockHTTP.AssertExpectations(t)
}

func TestGetWeatherAlerts_NoAlerts(t *testing.T) {
	// Load test fixture without alerts
	fixtureData := loadTestFixture(t, "seattle_alerts_empty.json")

	// Create mock HTTP client
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(200, fixtureData), nil)

	// Create client with mock
	client := NewClientWithHTTPDoer("test-api-key", "https://api.openweathermap.org", mockHTTP)

	coordinates := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}

	// Call GetWeatherAlerts
	alerts, err := client.GetWeatherAlerts(context.Background(), coordinates)

	// Verify results
	require.NoError(t, err)
	assert.Len(t, alerts, 0, "Should have no alerts")

	mockHTTP.AssertExpectations(t)
}

func TestGetWeatherAlerts_RateLimitError(t *testing.T) {
	// Create mock HTTP client that returns 429
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(429, `{"message": "Rate limit exceeded"}`), nil)

	client := NewClientWithHTTPDoer("test-api-key", "https://api.openweathermap.org", mockHTTP)
	coordinates := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}

	// Call GetWeatherAlerts
	alerts, err := client.GetWeatherAlerts(context.Background(), coordinates)

	// Verify error handling
	assert.Error(t, err)
	assert.Nil(t, alerts)
	assert.Contains(t, err.Error(), "rate limit exceeded")

	mockHTTP.AssertExpectations(t)
}

func TestGetWeatherAlerts_APIError(t *testing.T) {
	// Create mock HTTP client that returns 401 (invalid API key)
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(401, `{"cod": 401, "message": "Invalid API key"}`), nil)

	client := NewClientWithHTTPDoer("invalid-key", "https://api.openweathermap.org", mockHTTP)
	coordinates := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}

	// Call GetWeatherAlerts
	alerts, err := client.GetWeatherAlerts(context.Background(), coordinates)

	// Verify error handling
	assert.Error(t, err)
	assert.Nil(t, alerts)
	assert.Contains(t, err.Error(), "alerts API error 401")

	mockHTTP.AssertExpectations(t)
}

func TestGetWeatherAlerts_RequestFormat(t *testing.T) {
	// Create mock HTTP client that captures the request
	var capturedRequest *http.Request
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Run(func(args mock.Arguments) {
		capturedRequest = args.Get(0).(*http.Request)
	}).Return(createMockResponse(200, loadTestFixture(t, "seattle_alerts_empty.json")), nil)

	client := NewClientWithHTTPDoer("test-api-key", "https://api.openweathermap.org", mockHTTP)

	// Use high precision coordinates to test formatting
	coordinates := &api.Coordinates{
		Latitude:  47.606209,
		Longitude: -122.332100,
	}

	// Call GetWeatherAlerts
	_, err := client.GetWeatherAlerts(context.Background(), coordinates)
	require.NoError(t, err)

	// Verify request was properly formatted
	require.NotNil(t, capturedRequest)
	assert.Equal(t, "GET", capturedRequest.Method)
	assert.Contains(t, capturedRequest.URL.Path, "/data/3.0/onecall")

	// Verify query parameters
	query := capturedRequest.URL.Query()
	assert.Equal(t, "47.606209", query.Get("lat"))
	assert.Equal(t, "-122.332100", query.Get("lon"))
	assert.Equal(t, "test-api-key", query.Get("appid"))
	assert.Equal(t, "minutely,hourly,daily", query.Get("exclude"))

	mockHTTP.AssertExpectations(t)
}

func TestGetWeatherAlerts_AlertIDGeneration(t *testing.T) {
	// Test that alert IDs are generated consistently and uniquely
	fixtureData := loadTestFixture(t, "seattle_alerts_test.json")

	mockHTTP := &MockHTTPDoer{}
	// Set up mock to create fresh response for each call (bodies can only be read once)
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Run(func(args mock.Arguments) {
		// Do nothing, just to set up expectation
	}).Return(createMockResponse(200, fixtureData), nil).Once()

	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Run(func(args mock.Arguments) {
		// Do nothing, just to set up expectation
	}).Return(createMockResponse(200, fixtureData), nil).Once()

	client := NewClientWithHTTPDoer("test-api-key", "https://api.openweathermap.org", mockHTTP)
	coordinates := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}

	// Call GetWeatherAlerts multiple times
	alerts1, err1 := client.GetWeatherAlerts(context.Background(), coordinates)
	require.NoError(t, err1)

	alerts2, err2 := client.GetWeatherAlerts(context.Background(), coordinates)
	require.NoError(t, err2)

	// Verify that IDs are generated consistently (same input = same ID)
	require.Len(t, alerts1, 2)
	require.Len(t, alerts2, 2)
	assert.Equal(t, alerts1[0].Id, alerts2[0].Id, "Wind Advisory ID should be consistent")
	assert.Equal(t, alerts1[1].Id, alerts2[1].Id, "Flood Watch ID should be consistent")

	// Verify IDs contain meaningful components
	windAlertID := alerts1[0].Id
	assert.Contains(t, windAlertID, "NWS Seattle")
	assert.Contains(t, windAlertID, "Wind Advisory")
	assert.Contains(t, windAlertID, "1694880000") // Start timestamp

	mockHTTP.AssertExpectations(t)
}

func TestGetWeatherAlerts_InvalidJSON(t *testing.T) {
	// Create mock HTTP client that returns invalid JSON
	mockHTTP := &MockHTTPDoer{}
	mockHTTP.On("Do", mock.AnythingOfType("*http.Request")).Return(
		createMockResponse(200, `{"invalid": json}`), nil)

	client := NewClientWithHTTPDoer("test-api-key", "https://api.openweathermap.org", mockHTTP)
	coordinates := &api.Coordinates{Latitude: 47.6062, Longitude: -122.3321}

	// Call GetWeatherAlerts
	alerts, err := client.GetWeatherAlerts(context.Background(), coordinates)

	// Verify error handling
	assert.Error(t, err)
	assert.Nil(t, alerts)
	assert.Contains(t, err.Error(), "failed to decode alerts response")

	mockHTTP.AssertExpectations(t)
}
