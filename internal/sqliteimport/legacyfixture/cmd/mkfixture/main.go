// Command mkfixture writes a legacy-shaped SQLite grid.db (schema v14) of the
// size of the production file, for gridctl import and performance work:
//
//	go run ./internal/sqliteimport/legacyfixture/cmd/mkfixture -out /path/grid.db [-small]
//
// Deterministic (fixed seed). See notes on the P5 performance pass.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/sierragridteam/sierra-data/internal/sqliteimport/legacyfixture"
)

func main() {
	out := flag.String("out", "", "path of the SQLite file to create")
	small := flag.Bool("small", false, "the small test fixture instead of production scale")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mkfixture -out <path> [-small]")
		os.Exit(2)
	}
	spec := legacyfixture.ProductionScale()
	if *small {
		spec = legacyfixture.Small()
	}
	sum, err := legacyfixture.Build(context.Background(), *out, spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkfixture:", err)
		os.Exit(1)
	}
	fmt.Println("rows:", sum.Rows)
}
