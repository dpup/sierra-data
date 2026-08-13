// Command test-pge is the CLI diagnostic for the PG&E power source (peer of
// test-google/test-caltrans/test-weather/test-meshcore). It hits the live,
// undocumented PG&E ArcGIS services and prints what the poller would see:
//
//   - the outage feed, joined points + affected-area polygons, with the
//     severity and category the normalizer would assign;
//   - the PSPS coverage layer, grouped the way the normalizer groups it;
//   - PG&E's own ETL stamp for the outage service, and whether the freshness
//     gate would consider the feed FROZEN.
//
// That last one is the reason this tool exists. These endpoints publish no
// contract and no version, so the failure to watch for is not a 500 — it is the
// feed quietly freezing while still answering 200. Run this before trusting a
// quiet layer.
//
// Flags:
//
//	--bounds=minLat,minLng,maxLat,maxLng   query box (default: the service area)
//	--stale=1h                             freshness gate threshold
//	--json                                 dump raw normalized structs
//
// Example:
//
//	./bin/test-pge --bounds=37.87,-120.72,38.59,-119.89
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dpup/sierra-data/internal/clients/pge"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/ingest"
)

func main() {
	boundsFlag := flag.String("bounds", "37.87,-120.72,38.59,-119.89",
		"minLat,minLng,maxLat,maxLng (default: the Ebbetts Pass service area)")
	staleFlag := flag.Duration("stale", time.Hour, "freshness gate: max age of PG&E's outage ETL stamp")
	asJSON := flag.Bool("json", false, "dump the normalized structs as JSON")
	flag.Parse()

	b, err := parseBounds(*boundsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad --bounds: %v\n", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := pge.NewClient()
	failed := false

	fmt.Printf("PG&E service root: %s\n", pge.DefaultBaseURL)
	fmt.Printf("Query box: lat %.4f..%.4f, lng %.4f..%.4f\n\n", b.MinLatitude, b.MaxLatitude, b.MinLongitude, b.MaxLongitude)

	// --- freshness first: it decides how much to trust everything below ---
	fmt.Println("== upstream ETL stamp (outage service) ==")
	stamp, err := client.GetOutagesLastUpdate(ctx)
	switch {
	case err != nil:
		// The poller fails OPEN here (the gate is an extra signal on top of an
		// already-successful fetch), so this is a warning, not an exit code.
		fmt.Printf("  UNREADABLE: %v\n", err)
		fmt.Println("  -> the poller would skip the staleness gate and serve the feed ungated")
	default:
		age := time.Since(stamp).Round(time.Second)
		fmt.Printf("  last refreshed: %s (%s ago)\n", stamp.Format(time.RFC3339), age)
		if age > *staleFlag {
			failed = true
			fmt.Printf("  FROZEN: older than %s — the poller would record `pge` as failing,\n", *staleFlag)
			fmt.Println("          skip its disappearance sweep, and degrade the layer to STALE")
		} else {
			fmt.Printf("  fresh (gate: %s)\n", *staleFlag)
		}
	}

	// --- outages ---
	fmt.Println("\n== outages ==")
	outages, err := client.GetOutages(ctx, b)
	if err != nil {
		failed = true
		fmt.Printf("  FETCH FAILED: %v\n", err)
	} else {
		fmt.Printf("  %d outage(s) in box\n", len(outages))
		sort.Slice(outages, func(i, j int) bool {
			return outages[i].CustomersAffected > outages[j].CustomersAffected
		})
		for _, o := range outages {
			planned := o.Planned()
			category := "unplanned"
			if planned {
				category = "planned"
			}
			geom := "point"
			if o.HasPolygon {
				geom = "polygon"
			}
			fmt.Printf("  pge:%-8s %-8s %-9s %5d cust  %-14s %-18s %s\n",
				o.ID,
				hazards.SeverityFromPowerOutage(o.CustomersAffected, planned),
				category,
				o.CustomersAffected,
				geom,
				truncate(o.CrewStatus, 18),
				truncate(o.Cause, 20))
		}
		if len(outages) == 0 {
			fmt.Println("  (a clean empty result — no outages in box, NOT an error)")
		}
		if *asJSON {
			dump(outages)
		}
	}

	// --- PSPS ---
	fmt.Println("\n== PSPS coverage ==")
	areas, err := client.GetPSPSAreas(ctx, b)
	if err != nil {
		failed = true
		fmt.Printf("  FETCH FAILED: %v\n", err)
	} else {
		fmt.Printf("  %d coverage polygon row(s) in box\n", len(areas))
		// Group through the poller's OWN key function, so the ids printed here
		// are the ids that would be stored — including on the degenerate rows
		// the key's fallbacks exist for.
		groups := map[string][]pge.PSPSArea{}
		var order []string
		for _, a := range areas {
			key := ingest.PSPSGroupKey(a)
			if _, seen := groups[key]; !seen {
				order = append(order, key)
			}
			groups[key] = append(groups[key], a)
		}
		for _, key := range order {
			g := groups[key]
			// The poller stores the group's WORST stage, not the first row's, so
			// read through the same picker — otherwise this tool reports Watch
			// where the poller would store Warning, in exactly the degenerate
			// case it exists to diagnose.
			a := ingest.PSPSRepresentative(g)
			fmt.Printf("  psps:%-24s %-8s %-8s %6d cust / %5d medical baseline  %d row(s)\n",
				key, hazards.SeverityFromPSPSStage(a.Stage), a.Stage,
				a.CustomersAffected, a.MedicalBaselineAffected, len(g))
			fmt.Printf("      de-energization %s .. %s\n",
				fmtTime(a.DeEnergizationStart), fmtTime(a.DeEnergizationEnd))
			if !hazards.PSPSStageRecognized(a.Stage) {
				fmt.Printf("      NOTE unrecognized stage %q — classified conservatively as SEVERE\n", a.Stage)
			}
		}
		if len(areas) == 0 {
			fmt.Println("  (empty is the NORMAL state outside a shutoff event — this layer is active-only)")
		}
		if *asJSON {
			dump(areas)
		}
	}

	if failed {
		fmt.Println("\nOne or more checks failed — see above.")
		os.Exit(1)
	}
	fmt.Println("\nAll checks passed.")
}

func parseBounds(s string) (pge.Bounds, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return pge.Bounds{}, fmt.Errorf("want minLat,minLng,maxLat,maxLng, got %d values", len(parts))
	}
	v := make([]float64, 4)
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return pge.Bounds{}, err
		}
		v[i] = f
	}
	return pge.Bounds{MinLatitude: v[0], MinLongitude: v[1], MaxLatitude: v[2], MaxLongitude: v[3]}, nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "(none)"
	}
	return t.Format(time.RFC3339)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func dump(v any) {
	b, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "json: %v\n", err)
		return
	}
	fmt.Printf("  %s\n", b)
}
