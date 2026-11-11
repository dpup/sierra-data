package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// planNode is one node of EXPLAIN (FORMAT JSON) output.
type planNode struct {
	NodeType  string     `json:"Node Type"`
	Index     string     `json:"Index Name"`
	Relation  string     `json:"Relation Name"`
	Alias     string     `json:"Alias"`
	Output    []string   `json:"Output"`
	Plans     []planNode `json:"Plans"`
	Strategy  string     `json:"Strategy"`
	SortKey   []string   `json:"Sort Key"`
	Subplan   string     `json:"Subplan Name"`
	ParentRel string     `json:"Parent Relationship"`
}

// explain returns the plan PostgreSQL picks for query. With noSeqScan the
// planner is told to avoid sequential scans, bitmap scans and sorts (SET
// LOCAL enable_* = off) so a test on a handful of rows still shows which
// index CAN serve the query: at test sizes a seq scan and a sort are always
// cheapest, and the property worth pinning is that an index exists that
// matches the statement's filter and ORDER BY. A disabled node type still
// appears when nothing else can do the job, which is what a test catches.
func explain(t *testing.T, s *Store, noSeqScan bool, query string, args ...any) planNode {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	if noSeqScan {
		for _, knob := range []string{"enable_seqscan", "enable_bitmapscan", "enable_sort"} {
			_, err = tx.ExecContext(ctx, `SET LOCAL `+knob+` = off`)
			require.NoError(t, err)
		}
	}
	var raw string
	require.NoError(t, tx.QueryRowContext(ctx, "EXPLAIN (VERBOSE, FORMAT JSON) "+query, args...).Scan(&raw))
	var doc []struct {
		Plan planNode `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &doc), raw)
	require.Len(t, doc, 1)
	return doc[0].Plan
}

// walk visits n and every node beneath it.
func (n planNode) walk(fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}

// find returns the first node (depth first) matching pred.
func (n planNode) find(pred func(planNode) bool) (planNode, bool) {
	if pred(n) {
		return n, true
	}
	for _, c := range n.Plans {
		if f, ok := c.find(pred); ok {
			return f, true
		}
	}
	return planNode{}, false
}

// indexes lists every index the plan reads.
func (n planNode) indexes() []string {
	var out []string
	n.walk(func(p planNode) {
		if p.Index != "" {
			out = append(out, p.Index)
		}
	})
	return out
}

// String renders the tree compactly for failure messages.
func (n planNode) String() string {
	var sb strings.Builder
	var rec func(planNode, int)
	rec = func(p planNode, depth int) {
		fmt.Fprintf(&sb, "%s%s", strings.Repeat("  ", depth), p.NodeType)
		if p.Relation != "" {
			fmt.Fprintf(&sb, " on %s %s", p.Relation, p.Alias)
		} else if p.Alias != "" {
			fmt.Fprintf(&sb, " %s", p.Alias)
		}
		if p.Index != "" {
			fmt.Fprintf(&sb, " using %s", p.Index)
		}
		if len(p.SortKey) > 0 {
			fmt.Fprintf(&sb, " sort %v", p.SortKey)
		}
		sb.WriteString("\n")
		for _, c := range p.Plans {
			rec(c, depth+1)
		}
	}
	rec(n, 0)
	return sb.String()
}
