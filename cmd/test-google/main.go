package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/clients/google"
	"github.com/dpup/sierra-data/internal/config"
)

func main() {
	var (
		apiKey     = flag.String("api-key", "", "Google Routes API key (or set PF__GOOGLE_ROUTES__API_KEY env var)")
		configFile = flag.String("config", "", "Path to prefab.yaml config file (optional)")
		originStr  = flag.String("origin", "38.067400,-120.540200", "Origin coordinates (lat,lon)")
		destStr    = flag.String("dest", "38.139117,-120.456111", "Destination coordinates (lat,lon)")
		help       = flag.Bool("help", false, "Show help")
	)
	flag.Parse()

	if *help {
		fmt.Printf("Google Routes API Test Tool\n\n")
		fmt.Printf("Tests the Google Routes API client implementation.\n\n")
		fmt.Printf("Usage: %s [options]\n\n", os.Args[0])
		fmt.Printf("Options:\n")
		flag.PrintDefaults()
		fmt.Printf("\nExamples:\n")
		fmt.Printf("  %s -api-key=YOUR_KEY\n", os.Args[0])
		fmt.Printf("  %s -origin=\"37.7749,-122.4194\" -dest=\"34.0522,-118.2437\"\n", os.Args[0])
		fmt.Printf("  %s --config=prefab.yaml\n", os.Args[0])
		fmt.Printf("  PF__GOOGLE_ROUTES__API_KEY=your_key %s\n", os.Args[0])
		return
	}

	// Get API key from flag, config file, or environment
	key := *apiKey

	// If config file is provided, load configuration using shared LoadConfig
	if *configFile != "" {
		// For now, the shared LoadConfig always loads from the default prefab.yaml
		// The --config flag is supported but will use the shared configuration loading
		fmt.Printf("Loading configuration from shared config system\n")
		appConfig := config.LoadConfig()
		if key == "" && appConfig.GoogleRoutes.APIKey != "" {
			key = appConfig.GoogleRoutes.APIKey
			fmt.Printf("Using API key from configuration\n")
		}
	}

	// Fall back to environment variables
	if key == "" {
		key = os.Getenv("PF__GOOGLE_ROUTES__API_KEY")
		if key == "" {
			key = os.Getenv("GOOGLE_ROUTES_API_KEY") // fallback for backward compatibility
		}
	}

	if key == "" {
		log.Fatal("Google Routes API key required. Use -api-key flag, --config flag, or PF__GOOGLE_ROUTES__API_KEY env var")
	}

	// Parse coordinates
	var originLat, originLon, destLat, destLon float64
	_, err := fmt.Sscanf(*originStr, "%f,%f", &originLat, &originLon)
	if err != nil {
		log.Fatalf("Invalid origin coordinates: %v", err)
	}

	_, err = fmt.Sscanf(*destStr, "%f,%f", &destLat, &destLon)
	if err != nil {
		log.Fatalf("Invalid destination coordinates: %v", err)
	}

	fmt.Printf("Google Routes API Test\n")
	fmt.Printf("======================\n")
	fmt.Printf("Origin: %.6f, %.6f\n", originLat, originLon)
	fmt.Printf("Destination: %.6f, %.6f\n", destLat, destLon)
	fmt.Printf("API Key: %s...\n", key[:min(len(key), 10)])
	fmt.Printf("\n")

	// Create client and test
	client := google.NewClient(key)

	// Create coordinate structures
	origin := &api.Coordinates{
		Latitude:  originLat,
		Longitude: originLon,
	}
	destination := &api.Coordinates{
		Latitude:  destLat,
		Longitude: destLon,
	}

	fmt.Printf("Testing ComputeRoutes...\n")
	route, err := client.ComputeRoutes(context.Background(), origin, destination)
	if err != nil {
		log.Fatalf("ComputeRoutes failed: %v", err)
	}

	fmt.Printf("✅ ComputeRoutes successful!\n")
	fmt.Printf("Distance: %.2f km\n", float64(route.DistanceMeters)/1000.0)
	fmt.Printf("Duration: %.1f minutes\n", float64(route.DurationSeconds)/60.0)
	fmt.Printf("Polyline: %s...\n", route.Polyline[:min(len(route.Polyline), 50)])

	fmt.Printf("\n🎉 All Google Routes API tests passed!\n")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
