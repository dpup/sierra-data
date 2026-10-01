package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dpup/sierra-data/internal/clients/cwwp2"
)

// testCWWP2 probes the CWWP2 portal live for one district: what the server's
// chain_control layer would see (and whether it would call the feed frozen),
// what the message signs are showing, and the lane-closure feed's lifecycle
// breakdown.
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

	testCWWP2MessageSigns(ctx, c, district)
	fmt.Println()

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

// testCWWP2MessageSigns reports the district's changeable message signs:
// display modes, freshness, signs whose message the parser couldn't read, and
// each distinct message with the signs showing it. Grouping makes boilerplate
// obvious — a safety campaign is on dozens of signs at once — and leaves the
// one-off operational messages at the bottom.
func testCWWP2MessageSigns(ctx context.Context, c *cwwp2.Client, district int) {
	signs, err := c.MessageSigns(ctx, district)
	if err != nil {
		fmt.Printf("❌ message signs (%s): %v\n", c.FeedURL(district, "cms", "cms"), err)
		return
	}
	displays := map[string]int{}
	inService := 0
	var newest time.Time
	byText := map[string][]cwwp2.MessageSign{}
	var unknown []cwwp2.MessageSign
	for _, s := range signs {
		displays[s.Display.String()]++
		if s.InService {
			inService++
		}
		if s.RecordedAt.After(newest) {
			newest = s.RecordedAt
		}
		if s.Display == cwwp2.DisplayUnknown {
			unknown = append(unknown, s)
		}
		if t := s.Text(); t != "" {
			byText[t] = append(byText[t], s)
		}
	}
	fmt.Printf("✅ %d message signs, %d in service; displays %v\n", len(signs), inService, displays)
	age := time.Since(newest).Round(time.Second)
	verdict := "fresh"
	if age > cwwp2.DefaultStaleAfter {
		verdict = "STALE — a client with the default StaleAfter would fail this feed"
	}
	fmt.Printf("   newest record %s (%s old, %s)\n", newest.Format(time.RFC3339), age, verdict)
	for _, s := range unknown {
		fmt.Printf("   ⚠️  UNKNOWN display %q at %s %s (%s, in service: %t)\n", s.RawDisplay, s.Location.Route, s.Location.Name, s.ID, s.InService)
	}

	texts := make([]string, 0, len(byText))
	for t := range byText {
		texts = append(texts, t)
	}
	sort.Slice(texts, func(i, j int) bool {
		if a, b := len(byText[texts[i]]), len(byText[texts[j]]); a != b {
			return a > b
		}
		return texts[i] < texts[j]
	})
	for _, t := range texts {
		group := byText[t]
		var latest time.Time
		names := make([]string, 0, 3)
		for i, s := range group {
			if s.MessageSince.After(latest) {
				latest = s.MessageSince
			}
			if i < 3 {
				names = append(names, s.Location.Name)
			}
		}
		more := ""
		if len(group) > len(names) {
			more = fmt.Sprintf(" +%d more", len(group)-len(names))
		}
		since := "unreported"
		if !latest.IsZero() {
			since = latest.Format("2006-01-02 15:04")
		}
		fmt.Printf("   💬 %3d× %q (latest change %s)\n         %s%s\n", len(group), t, since, strings.Join(names, "; "), more)
	}
}
