package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/dpup/sierra-data/internal/clients/cwwp2"
)

// testCWWP2 probes the CWWP2 portal live for one district: what the server's
// chain_control layer would see (and whether it would call the feed frozen),
// the lane-closure feed's lifecycle breakdown, and the camera list.
func testCWWP2(ctx context.Context, district int) {
	fmt.Printf("🗂️  CWWP2 portal, district %d\n", district)
	fmt.Printf("---------------------------\n")
	c := cwwp2.NewClient()

	// Freshness is reported, not enforced, so a frozen file can still be read.
	c.StaleAfter = 0
	controls, err := c.ChainControls(ctx, district)
	if err != nil {
		fmt.Printf("❌ chain controls (%s): %v\n", c.FeedURL(district, "cc", "cc"), err)
	} else {
		levels := map[string]int{}
		var newest time.Time
		var active, unknown []cwwp2.ChainControl
		for _, cp := range controls {
			levels[cp.Level.String()]++
			if cp.RecordedAt.After(newest) {
				newest = cp.RecordedAt
			}
			switch {
			case cp.Level.Active():
				active = append(active, cp)
			case cp.Level == cwwp2.LevelUnknown:
				unknown = append(unknown, cp)
			}
		}
		fmt.Printf("✅ %d checkpoints; levels %v\n", len(controls), levels)
		age := time.Since(newest).Round(time.Second)
		verdict := "fresh"
		if age > cwwp2.DefaultStaleAfter {
			verdict = "STALE — the server would fail this feed"
		}
		fmt.Printf("   newest record %s (%s old, %s)\n", newest.Format(time.RFC3339), age, verdict)
		for _, cp := range active {
			fmt.Printf("   ⛓️  %s %-5s %-40s %s since %s\n", cp.Location.Route, cp.Location.Direction, cp.Location.Name, cp.Level, cp.StatusSince.Format("2006-01-02 15:04"))
		}
		for _, cp := range unknown {
			fmt.Printf("   ⚠️  UNRECOGNIZED status %q at %s %s (%s)\n", cp.RawStatus, cp.Location.Route, cp.Location.Name, cp.ID)
		}
	}
	fmt.Println()

	probeLaneClosures(ctx, c, district)
	fmt.Println()
	probeCameras(ctx, c, district)
}

// probeLaneClosures prints the lane-closure feed's lifecycle breakdown by county.
func probeLaneClosures(ctx context.Context, c *cwwp2.Client, district int) {
	closures, err := c.LaneClosures(ctx, district)
	if err != nil {
		fmt.Printf("❌ lane closures (%s): %v\n", c.FeedURL(district, "lcs", "lcs"), err)
		return
	}
	now := time.Now()
	phases := map[string]int{}
	byCounty := map[string]map[string]int{}
	for _, lc := range closures {
		ph := lc.PhaseAt(now).String()
		phases[ph]++
		county := lc.Begin.County
		if byCounty[county] == nil {
			byCounty[county] = map[string]int{}
		}
		byCounty[county][ph]++
	}
	fmt.Printf("✅ %d lane-closure windows; phases %v\n", len(closures), phases)
	counties := make([]string, 0, len(byCounty))
	for k := range byCounty {
		counties = append(counties, k)
	}
	sort.Strings(counties)
	for _, k := range counties {
		fmt.Printf("   %-14s %v\n", k, byCounty[k])
	}
}

// probeCameras prints the district's camera list as the camera directory sees
// it: how many it would serve, and which it drops. The geography filter needs
// the place directory, so it is not applied here; cameras are listed by county.
func probeCameras(ctx context.Context, c *cwwp2.Client, district int) {
	cams, err := c.Cameras(ctx, district)
	if err != nil {
		fmt.Printf("❌ cameras (%s): %v\n", c.FeedURL(district, "cctv", "cctv"), err)
		return
	}
	byCounty := map[string]int{}
	var outOfService []cwwp2.Camera
	noImage, imageOnly := 0, 0
	for _, cam := range cams {
		switch {
		case !cam.InService:
			outOfService = append(outOfService, cam)
		case cam.ImageURL == "" || !cam.Location.HasPosition:
			noImage++
		default:
			byCounty[cam.Location.County]++
			if cam.StreamURL == "" {
				imageOnly++
			}
		}
	}
	served := len(cams) - len(outOfService) - noImage
	fmt.Printf("✅ %d cameras; %d servable (%d image-only), %d out of service, %d without a position or https image\n",
		len(cams), served, imageOnly, len(outOfService), noImage)
	counties := make([]string, 0, len(byCounty))
	for k := range byCounty {
		counties = append(counties, k)
	}
	sort.Strings(counties)
	for _, k := range counties {
		fmt.Printf("   %-14s %d\n", k, byCounty[k])
	}
	for _, cam := range outOfService {
		fmt.Printf("   📵 out of service: %s %s (%s)\n", cam.Location.Route, cam.Name, cam.ID)
	}
}
