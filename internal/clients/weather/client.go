package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	api "github.com/dpup/info.ersn.net/server/api/v1"
)

// HTTPDoer interface for HTTP clients (for testability)
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client provides access to OpenWeatherMap API
// Implementation per research.md lines 68-82
type Client struct {
	apiKey     string
	httpClient HTTPDoer
	baseURL    string
}

// NewClient creates a new OpenWeatherMap API client
func NewClient(apiKey string) *Client {
	return &Client{
		apiKey:  apiKey,
		baseURL: "https://api.openweathermap.org",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewClientWithHTTPDoer creates a new client with a custom HTTP client (for testing)
func NewClientWithHTTPDoer(apiKey, baseURL string, httpClient HTTPDoer) *Client {
	return &Client{
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: httpClient,
	}
}

// GetCurrentWeather retrieves current weather conditions for coordinates
// Endpoint per research.md line 92
func (c *Client) GetCurrentWeather(ctx context.Context, coordinates *api.Coordinates) (*api.WeatherData, error) {
	// Build URL with query parameters
	params := url.Values{}
	params.Set("lat", fmt.Sprintf("%.6f", coordinates.Latitude))
	params.Set("lon", fmt.Sprintf("%.6f", coordinates.Longitude))
	params.Set("appid", c.apiKey)
	params.Set("units", "metric") // Get temperature in Celsius

	requestURL := fmt.Sprintf("%s/data/2.5/weather?%s", c.baseURL, params.Encode())

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Execute request with rate limiting awareness (60/minute from research.md line 99)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Handle rate limiting and errors per research.md line 100
	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate limit exceeded (60/minute)")
	}
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("invalid API key")
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var response OpenWeatherCurrentResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return c.processCurrentWeatherResponse(response)
}

// GetWeatherAlerts retrieves weather alerts using One Call API 3.0.
//
// NOT used by the server: One Call 3.0 has a 1,000 calls/day free cap, and for
// US locations its alerts are relabeled NWS data — the server sources alerts
// from api.weather.gov directly (see internal/services/weather_nws.go). This
// method is kept only for the cmd/test-weather diagnostic CLI; do not wire it
// back into a refresh path.
func (c *Client) GetWeatherAlerts(ctx context.Context, coordinates *api.Coordinates) ([]*api.WeatherAlert, error) {
	// Build URL for One Call API with alerts
	params := url.Values{}
	params.Set("lat", fmt.Sprintf("%.6f", coordinates.Latitude))
	params.Set("lon", fmt.Sprintf("%.6f", coordinates.Longitude))
	params.Set("appid", c.apiKey)
	params.Set("exclude", "minutely,hourly,daily") // Only get current + alerts

	requestURL := fmt.Sprintf("%s/data/3.0/onecall?%s", c.baseURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create alerts request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute alerts request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate limit exceeded (60/minute)")
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("alerts API error %d: %s", resp.StatusCode, string(body))
	}

	var response OpenWeatherOneCallResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode alerts response: %w", err)
	}

	return c.processWeatherAlerts(response.Alerts)
}

// processCurrentWeatherResponse converts OpenWeatherMap response to our WeatherData format
// Mapping per data-model.md lines 123-146
func (c *Client) processCurrentWeatherResponse(response OpenWeatherCurrentResponse) (*api.WeatherData, error) {
	// Extract primary weather condition
	var weatherMain, weatherDescription, weatherIcon string
	if len(response.Weather) > 0 {
		weatherMain = response.Weather[0].Main
		weatherDescription = response.Weather[0].Description
		weatherIcon = response.Weather[0].Icon
	}

	return &api.WeatherData{
		LocationId:           "", // Will be set by calling service
		LocationName:         response.Name,
		WeatherMain:          weatherMain,
		WeatherDescription:   weatherDescription,
		WeatherIcon:          weatherIcon,
		TemperatureCelsius:   int32(response.Main.Temp),      // Round to int
		FeelsLikeCelsius:     int32(response.Main.FeelsLike), // Round to int
		HumidityPercent:      response.Main.Humidity,
		WindSpeedKmh:         int32(response.Wind.Speed * 3.6), // Convert m/s to km/h
		WindDirectionDegrees: response.Wind.Deg,
		VisibilityKm:         int32(response.Visibility / 1000), // Convert meters to km
		Alerts:               nil,                               // Alerts fetched separately
	}, nil
}

// processWeatherAlerts converts OpenWeatherMap alerts to our WeatherAlert format
// Mapping per data-model.md lines 169-181
func (c *Client) processWeatherAlerts(alerts []OpenWeatherAlert) ([]*api.WeatherAlert, error) {
	var processedAlerts []*api.WeatherAlert

	for _, alert := range alerts {
		// Generate unique ID from sender + event + start time
		id := fmt.Sprintf("%s_%s_%d", alert.SenderName, alert.Event, alert.Start)

		processedAlert := &api.WeatherAlert{
			Id:          id,
			SenderName:  alert.SenderName,
			Event:       alert.Event,
			Description: alert.Description,
			Tags:        alert.Tags,
		}
		if alert.Start > 0 {
			processedAlert.StartTime = timestamppb.New(time.Unix(alert.Start, 0).UTC())
		}
		if alert.End > 0 {
			processedAlert.EndTime = timestamppb.New(time.Unix(alert.End, 0).UTC())
		}

		processedAlerts = append(processedAlerts, processedAlert)
	}

	return processedAlerts, nil
}

// OpenWeatherCurrentResponse represents the current weather API response
type OpenWeatherCurrentResponse struct {
	Coord      OpenWeatherCoord     `json:"coord"`
	Weather    []OpenWeatherWeather `json:"weather"`
	Main       OpenWeatherMain      `json:"main"`
	Wind       OpenWeatherWind      `json:"wind"`
	Clouds     OpenWeatherClouds    `json:"clouds"`
	Visibility int32                `json:"visibility"`
	Name       string               `json:"name"`
	Dt         int64                `json:"dt"`
}

// OpenWeatherOneCallResponse represents One Call API response with alerts
type OpenWeatherOneCallResponse struct {
	Lat    float64            `json:"lat"`
	Lon    float64            `json:"lon"`
	Alerts []OpenWeatherAlert `json:"alerts,omitempty"`
}

// OpenWeatherCoord represents coordinates in response
type OpenWeatherCoord struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// OpenWeatherWeather represents weather condition
type OpenWeatherWeather struct {
	Main        string `json:"main"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
}

// OpenWeatherMain represents main weather data
type OpenWeatherMain struct {
	Temp      float32 `json:"temp"`
	FeelsLike float32 `json:"feels_like"`
	Pressure  int32   `json:"pressure"`
	Humidity  int32   `json:"humidity"`
}

// OpenWeatherWind represents wind data
type OpenWeatherWind struct {
	Speed float32 `json:"speed"`
	Deg   int32   `json:"deg"`
}

// OpenWeatherClouds represents cloud cover
type OpenWeatherClouds struct {
	All int32 `json:"all"`
}

// OpenWeatherAlert represents weather alert from One Call API
// Structure per data-model.md lines 169-181
type OpenWeatherAlert struct {
	SenderName  string   `json:"sender_name"`
	Event       string   `json:"event"`
	Start       int64    `json:"start"`
	End         int64    `json:"end"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}
