package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/weather"
	"github.com/dpup/sierra-data/internal/config"
)

func main() {
	var (
		apiKey     = flag.String("api-key", "", "OpenWeatherMap API key (or set PF__OPENWEATHER__API_KEY env var)")
		configFile = flag.String("config", "", "Path to prefab.yaml config file (optional)")
		lat        = flag.Float64("lat", 38.139117, "Latitude for weather lookup")
		lon        = flag.Float64("lon", -120.456111, "Longitude for weather lookup")
		name       = flag.String("name", "Murphys, CA", "Location name for display")
		help       = flag.Bool("help", false, "Show help")
	)
	flag.Parse()

	if *help {
		fmt.Printf("OpenWeatherMap API Test Tool\n\n")
		fmt.Printf("Tests the OpenWeatherMap API client implementation.\n\n")
		fmt.Printf("Usage: %s [options]\n\n", os.Args[0])
		fmt.Printf("Options:\n")
		flag.PrintDefaults()
		fmt.Printf("\nExamples:\n")
		fmt.Printf("  %s -api-key=YOUR_KEY\n", os.Args[0])
		fmt.Printf("  %s -lat=37.7749 -lon=-122.4194 -name=\"San Francisco, CA\"\n", os.Args[0])
		fmt.Printf("  %s --config=prefab.yaml\n", os.Args[0])
		fmt.Printf("  PF__OPENWEATHER__API_KEY=your_key %s\n", os.Args[0])
		return
	}

	// Get API key from flag, config file, or environment
	key := *apiKey

	// If config file is provided, load configuration using shared LoadConfig
	if *configFile != "" {
		fmt.Printf("Loading configuration from shared config system\n")
		appConfig := config.LoadConfig()
		if key == "" && appConfig.OpenWeather.APIKey != "" {
			key = appConfig.OpenWeather.APIKey
			fmt.Printf("Using API key from configuration\n")
		}
	}

	// Fall back to environment variables
	if key == "" {
		key = os.Getenv("PF__OPENWEATHER__API_KEY")
		if key == "" {
			key = os.Getenv("OPENWEATHER_API_KEY") // fallback for backward compatibility
		}
	}

	if key == "" {
		log.Fatal("OpenWeatherMap API key required. Use -api-key flag, --config flag, or PF__OPENWEATHER__API_KEY env var")
	}

	fmt.Printf("OpenWeatherMap API Test\n")
	fmt.Printf("=======================\n")
	fmt.Printf("Location: %s\n", *name)
	fmt.Printf("Coordinates: %.6f, %.6f\n", *lat, *lon)
	fmt.Printf("API Key: %s...\n", key[:min(len(key), 10)])
	fmt.Printf("\n")

	// Create client and test
	client := weather.NewClient(key)
	ctx := context.Background()

	// Create coordinates structure
	coords := &api.Coordinates{
		Latitude:  *lat,
		Longitude: *lon,
	}

	// Test current weather
	fmt.Printf("Testing GetCurrentWeather...\n")
	current, err := client.GetCurrentWeather(ctx, coords)
	if err != nil {
		log.Fatalf("GetCurrentWeather failed: %v", err)
	}

	fmt.Printf("✅ GetCurrentWeather successful!\n")
	fmt.Printf("Temperature: %d°C (feels like %d°C)\n",
		current.TemperatureCelsius, current.FeelsLikeCelsius)
	fmt.Printf("Condition: %s\n", current.WeatherMain)
	fmt.Printf("Description: %s\n", current.WeatherDescription)
	fmt.Printf("Humidity: %.0f%%\n", float64(current.HumidityPercent))
	fmt.Printf("Wind: %d km/h\n", current.WindSpeedKmh)
	if current.VisibilityKm > 0 {
		fmt.Printf("Visibility: %d km\n", current.VisibilityKm)
	}
	fmt.Printf("\n")

	// Test weather alerts
	fmt.Printf("Testing GetWeatherAlerts...\n")
	alerts, err := client.GetWeatherAlerts(ctx, coords)
	if err != nil {
		log.Fatalf("GetWeatherAlerts failed: %v", err)
	}

	fmt.Printf("✅ GetWeatherAlerts successful!\n")
	if len(alerts) == 0 {
		fmt.Printf("No weather alerts found (this is normal)\n")
	} else {
		fmt.Printf("Weather alerts found: %d\n", len(alerts))
		for i, alert := range alerts {
			fmt.Printf("Alert %d:\n", i+1)
			fmt.Printf("  Event: %s\n", alert.Event)
			fmt.Printf("  Description: %s\n", truncateString(alert.Description, 100))
			fmt.Printf("  Sender: %s\n", alert.SenderName)
			if alert.GetStartTime() != nil {
				fmt.Printf("  Start: %s\n", alert.GetStartTime().AsTime().Format("2006-01-02 15:04:05"))
			}
			if alert.GetEndTime() != nil {
				fmt.Printf("  End: %s\n", alert.GetEndTime().AsTime().Format("2006-01-02 15:04:05"))
			}
			if len(alert.Tags) > 0 {
				fmt.Printf("  Tags: %v\n", alert.Tags)
			}
		}
	}
	fmt.Printf("\n")

	// Test with multiple locations
	fmt.Printf("Testing GetWeatherForLocations...\n")
	locations := []struct {
		Name string
		Lat  float64
		Lon  float64
	}{
		{"Murphys, CA", 38.139117, -120.456111},
		{"Arnold, CA", 38.265006, -120.333654},
		{"Bear Valley, CA", 38.461045, -120.042368},
	}

	for _, loc := range locations {
		fmt.Printf("Getting weather for %s...\n", loc.Name)
		locCoords := &api.Coordinates{
			Latitude:  loc.Lat,
			Longitude: loc.Lon,
		}
		weather, err := client.GetCurrentWeather(ctx, locCoords)
		if err != nil {
			fmt.Printf("  ❌ Error: %v\n", err)
			continue
		}
		fmt.Printf("  ✅ %s: %d°C, %s\n",
			loc.Name, weather.TemperatureCelsius, weather.WeatherMain)
	}

	fmt.Printf("\n🎉 All OpenWeatherMap API tests completed!\n")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
