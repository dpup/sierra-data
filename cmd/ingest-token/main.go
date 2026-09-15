// Command ingest-token mints a push-ingest credential for a reporter and, when
// asked, writes it into prefab.yaml.
//
// The asymmetry it exists to enforce: the TOKEN is printed once, to the
// terminal, and stored nowhere; only its SHA-256 HASH is written to the config.
// A hash of a 256-bit random token cannot be replayed or reversed, so it is safe
// in a public repository — which is what lets adding a reporter be an ordinary
// pull request. Doing this by hand means copying a 64-character hash between two
// windows and hoping you grabbed the right one of the two similar-looking hex
// strings on screen; getting it wrong produces a reporter that authenticates
// cleanly against nothing.
//
//	go run ./cmd/ingest-token                    # just mint a pair
//	go run ./cmd/ingest-token -reporter alan-pi  # mint and write it into prefab.yaml
//
// Re-running with an existing id ROTATES that reporter's token, leaving every
// other setting on the entry untouched.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/dpup/sierra-data/internal/pushingest"
)

// idPattern constrains a reporter id. It becomes a source-registry row id and
// appears verbatim in /api/v1/sources, so it lives under the same lowercase
// slug convention as every other source ("usgs", "calfire", "pge").
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$`)

func main() {
	var (
		id          = flag.String("reporter", "", "reporter id to add or rotate in prefab.yaml; omit to only print a pair")
		name        = flag.String("name", "", "human-readable reporter name (defaults to the id)")
		streams     = flag.String("streams", pushingest.MeshStream, "comma-separated streams to authorize")
		configPath  = flag.String("config", "prefab.yaml", "path to the config file to edit")
		staleAfter  = flag.String("stale-after", "30m", "silence before the reporter degrades and its sweep is suppressed")
		minInterval = flag.String("min-interval", "30s", "minimum gap between accepted reports")
		priority    = flag.Int("priority", 10, "identity tie-break among reporters; higher wins")
	)
	flag.Parse()

	token, err := mintToken()
	if err != nil {
		fail("could not generate a token: %v", err)
	}
	hash := pushingest.TokenHash(token)

	if *id == "" {
		printPair(token, hash)
		fmt.Println("  Nothing was written. Re-run with -reporter <id> to add it to prefab.yaml,")
		fmt.Println("  or paste the hash into grid.ingest.reporters[].tokenSha256 by hand.")
		fmt.Println()
		return
	}

	if !idPattern.MatchString(*id) {
		fail("reporter id %q must be a lowercase slug (letters, digits, hyphens) — it becomes a source id on /api/v1/sources", *id)
	}
	list := splitStreams(*streams)
	if len(list) == 0 {
		fail("no streams given; a reporter authorizes streams explicitly, never by default")
	}
	known := pushingest.KnownStreams()
	for _, s := range list {
		if !slices.Contains(known, s) {
			// Caught here rather than at startup: the server would accept the
			// config happily, and the operator would only learn about the typo
			// when their monitor started collecting 404s.
			fail("unknown stream %q; this build serves: %s", s, strings.Join(known, ", "))
		}
	}

	src, err := os.ReadFile(*configPath)
	if err != nil {
		fail("could not read %s: %v", *configPath, err)
	}
	displayName := *name
	if displayName == "" {
		displayName = *id
	}

	out, act, err := upsertReporter(string(src), reporterSpec{
		ID:          *id,
		Name:        displayName,
		Streams:     list,
		TokenSha256: hash,
		StaleAfter:  *staleAfter,
		MinInterval: *minInterval,
		Priority:    *priority,
	})
	if err != nil {
		fail("could not update %s: %v", *configPath, err)
	}

	// The one assertion worth making unconditionally: the point of this tool is
	// that the token never reaches a file that gets committed. Verify it rather
	// than trusting the code above to have done the right thing.
	if strings.Contains(out, token) {
		fail("refusing to write %s: the generated token appears in the output — this is a bug", *configPath)
	}

	info, err := os.Stat(*configPath)
	if err != nil {
		fail("could not stat %s: %v", *configPath, err)
	}
	if err := os.WriteFile(*configPath, []byte(out), info.Mode().Perm()); err != nil {
		fail("could not write %s: %v", *configPath, err)
	}

	printPair(token, hash)
	fmt.Printf("  %s reporter %q in %s (streams: %s)\n\n", act, *id, *configPath, strings.Join(list, ", "))
	if act == actionRotated {
		fmt.Println("  Rotated in place — every other setting on the entry was left alone.")
		fmt.Println("  The previous token stops working as soon as the server restarts, so")
		fmt.Println("  hand the new one over before you deploy.")
	}
	fmt.Println("  Next:")
	fmt.Printf("    1. Review the change:  git diff %s\n", *configPath)
	fmt.Println("    2. Send the TOKEN to the operator over a private channel. It is not")
	fmt.Println("       stored anywhere — if you lose it, rotate rather than go looking.")
	fmt.Println("    3. Commit the config. The hash is safe to publish; the token never is.")
	fmt.Println()
}

// mintToken generates 256 bits of entropy, hex-encoded.
func mintToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func printPair(token, hash string) {
	fmt.Println()
	fmt.Println("  token       (give to the reporter, stored nowhere else)")
	fmt.Println("    " + token)
	fmt.Println("  tokenSha256 (goes in prefab.yaml, safe to commit)")
	fmt.Println("    " + hash)
	fmt.Println()
	fmt.Println("  Test it:")
	fmt.Println("    curl -X POST http://localhost:8181/api/v1/ingest/" + pushingest.MeshStream + " \\")
	fmt.Println("      -H 'Authorization: Bearer " + token + "' \\")
	fmt.Println("      -H 'Content-Type: application/json' --data @report.json")
	fmt.Println()
}

func splitStreams(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// fail writes to stderr so a shell capturing stdout never mistakes an error for
// a credential.
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ingest-token: "+format+"\n", args...)
	os.Exit(1)
}
