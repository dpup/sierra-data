package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dpup/sierra-data/internal/clients/caltrans"
)

func main() {
	var (
		feedType = flag.String("feed", "all", "Feed type: all, chain, lanes, chp")
		offline  = flag.Bool("offline", false, "Use local test data instead of live feeds")
		help     = flag.Bool("help", false, "Show help")
	)
	flag.Parse()

	if *help {
		fmt.Printf("Caltrans KML Parser Test Tool\n\n")
		fmt.Printf("Tests the Caltrans KML feed parser implementation.\n\n")
		fmt.Printf("Usage: %s [options]\n\n", os.Args[0])
		fmt.Printf("Options:\n")
		flag.PrintDefaults()
		fmt.Printf("\nFeed types:\n")
		fmt.Printf("  all   - Test all feeds (default)\n")
		fmt.Printf("  chain - Chain control feed only\n")
		fmt.Printf("  lanes - Lane closures feed only\n")
		fmt.Printf("  chp   - CHP incidents feed only\n")
		fmt.Printf("\nExamples:\n")
		fmt.Printf("  %s\n", os.Args[0])
		fmt.Printf("  %s -feed=chain\n", os.Args[0])
		fmt.Printf("  %s -filter -lat=38.2 -lon=-120.3 -radius=25000\n", os.Args[0])
		fmt.Printf("  %s -offline  # Use local test data for faster testing\n", os.Args[0])
		return
	}

	fmt.Printf("Caltrans KML Parser Test\n")
	fmt.Printf("========================\n")
	fmt.Printf("Feed type: %s\n", *feedType)
	if *offline {
		fmt.Printf("Mode: Offline (using local test data)\n")
	} else {
		fmt.Printf("Mode: Online (using live feeds)\n")
	}
	fmt.Printf("\n")

	// Create parser
	var parser *caltrans.FeedParser
	if *offline {
		parser = createOfflineParser()
	} else {
		parser = caltrans.NewFeedParser()
	}
	ctx := context.Background()

	switch *feedType {
	case "chain":
		testChainControls(parser, ctx)
	case "lanes":
		testLaneClosures(parser, ctx)
	case "chp":
		testCHPIncidents(parser, ctx)
	case "all":
		testChainControls(parser, ctx)
		testLaneClosures(parser, ctx)
		testCHPIncidents(parser, ctx)

	default:
		log.Fatalf("Unknown feed type: %s", *feedType)
	}

	fmt.Printf("\n🎉 All Caltrans KML parser tests completed!\n")
}

// mockHTTPClient provides local KML file responses for testing
type mockHTTPClient struct {
	testDataDir string
}

func (m *mockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	var filename string
	switch req.URL.String() {
	case "https://quickmap.dot.ca.gov/data/lcs2way.kml":
		filename = "lane_closures.kml"
	case "https://quickmap.dot.ca.gov/data/chp-only.kml":
		filename = "chp_incidents.kml"
	case "https://quickmap.dot.ca.gov/data/cc.kml":
		filename = "chain_controls.kml"
	default:
		return &http.Response{
			StatusCode: 404,
			Body:       io.NopCloser(strings.NewReader("Not found")),
		}, nil
	}

	filePath := filepath.Join(m.testDataDir, filename)
	file, err := os.Open(filePath)
	if err != nil {
		return &http.Response{
			StatusCode: 500,
			Body:       io.NopCloser(strings.NewReader("Internal server error")),
		}, err
	}

	return &http.Response{
		StatusCode: 200,
		Body:       file,
	}, nil
}

func createOfflineParser() *caltrans.FeedParser {
	// Get the test data directory relative to the executable
	execDir, err := os.Executable()
	if err != nil {
		log.Printf("Warning: Could not determine executable path: %v", err)
		execDir = "."
	}

	// Look for test data relative to project root
	testDataDir := filepath.Join(filepath.Dir(execDir), "..", "tests", "testdata", "caltrans")

	// Also try relative to current working directory
	if _, err := os.Stat(testDataDir); err != nil {
		testDataDir = filepath.Join("tests", "testdata", "caltrans")
		if _, err := os.Stat(testDataDir); err != nil {
			log.Fatalf("Test data not found. Run from project root or ensure tests/testdata/caltrans/ exists")
		}
	}

	return &caltrans.FeedParser{
		HTTPClient: &mockHTTPClient{testDataDir: testDataDir},
	}
}

func testChainControls(parser *caltrans.FeedParser, ctx context.Context) {
	fmt.Printf("Testing Chain Controls feed...\n")

	incidents, err := parser.ParseChainControls(ctx)
	if err != nil {
		log.Fatalf("ParseChainControls failed: %v", err)
	}

	fmt.Printf("✅ Chain Controls successful!\n")
	fmt.Printf("Incidents found: %d\n", len(incidents))

	if len(incidents) > 0 {
		printSampleIncident("Chain Control", incidents[0])
	}
	fmt.Printf("\n")
}

func testLaneClosures(parser *caltrans.FeedParser, ctx context.Context) {
	fmt.Printf("Testing Lane Closures feed...\n")

	incidents, err := parser.ParseLaneClosures(ctx)
	if err != nil {
		log.Fatalf("ParseLaneClosures failed: %v", err)
	}

	fmt.Printf("✅ Lane Closures successful!\n")
	fmt.Printf("Incidents found: %d\n", len(incidents))

	if len(incidents) > 0 {
		printSampleIncident("Lane Closure", incidents[0])
	}
	fmt.Printf("\n")
}

func testCHPIncidents(parser *caltrans.FeedParser, ctx context.Context) {
	fmt.Printf("Testing CHP Incidents feed...\n")

	incidents, err := parser.ParseCHPIncidents(ctx)
	if err != nil {
		log.Fatalf("ParseCHPIncidents failed: %v", err)
	}

	fmt.Printf("✅ CHP Incidents successful!\n")
	fmt.Printf("Incidents found: %d\n", len(incidents))

	if len(incidents) > 0 {
		printSampleIncident("CHP Incident", incidents[0])
	}
	fmt.Printf("\n")
}

func printSampleIncident(label string, incident caltrans.CaltransIncident) {
	fmt.Printf("Sample %s:\n", label)
	fmt.Printf("  Name: %s\n", incident.Name)
	fmt.Printf("  Coordinates: %.6f, %.6f\n",
		incident.Coordinates.Latitude, incident.Coordinates.Longitude)
	fmt.Printf("  Status: %s\n", incident.ParsedStatus)
	fmt.Printf("  Description: %s\n", truncateString(incident.DescriptionText, 100))
	if len(incident.ParsedDates) > 0 {
		fmt.Printf("  Dates: %v\n", incident.ParsedDates)
	}
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
