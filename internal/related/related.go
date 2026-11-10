// Package related finds events that are about the same situation: a fire and
// its evacuations, a PG&E shutoff and the Red Flag Warning it ran under, a
// snowstorm and the closures inside it.
//
// It is a registry of rules (rules.go), applied at read time. A Rule pairs two
// kinds of event (side A and side B), says how far apart in time and space a
// pair may be, and judges each candidate pair, returning a reason and a
// confidence. The engine applies every rule from whichever side the subject is
// on, so a fire lists its evacuations and an evacuation points back to its
// fire. To add a relation, add a Rule to Rules; nothing else changes.
//
// Rules state what the sources say or how events coincide, never cause the
// sources do not state: "near" and "during", not "because of".
package related

import (
	"context"
	"math"
	"sort"
	"time"

	gridv1 "github.com/sierragridteam/sierra-data/api/grid/v1"
	"github.com/sierragridteam/sierra-data/internal/lib/geojson"
	"github.com/sierragridteam/sierra-data/internal/store"
)

// Confidence orders how strongly a relation is supported.
type Confidence int

const (
	// Context: coincidence in time and place worth showing beside the event,
	// claiming no link (an outage inside a High Wind Warning).
	Context Confidence = iota + 1
	// Low: plausible from time and place alone.
	Low
	// Medium: time and place plus a corroborating fact (PG&E's cause).
	Medium
	// High: a source names the other event.
	High
)

// Match is a rule's verdict on one pair.
type Match struct {
	Confidence Confidence
	Reason     string // one plain sentence, e.g. "Cal OES notes name the Ross Fire"
	// Score breaks ties for exclusive rules: lower is better (e.g. km apart).
	Score float64
}

// Side selects the events a rule pairs on one side.
type Side struct {
	Layer gridv1.Layer
	// Keep, when set, narrows the layer (e.g. only PSPS among POWER events).
	Keep func(*gridv1.Event) bool
}

func (s Side) has(ev *gridv1.Event) bool {
	return ev.GetLayer() == s.Layer && (s.Keep == nil || s.Keep(ev))
}

// Rule is one kind of relation.
type Rule struct {
	// Name is stable and shown to clients, e.g. "fire-evacuation-named".
	Name string
	A, B Side
	// Before and After bound B's start relative to A's span: B must start no
	// earlier than A.start-Before and no later than A.end+After.
	Before, After time.Duration
	// MaxKm bounds the gap between the two events' extents (bounding boxes).
	// 0 means no spatial bound beyond Judge's own test.
	MaxKm float64
	// ShapeA loads A's full geometry for Judge (e.g. an alert's area, to test
	// containment). Candidates are otherwise read without shapes.
	ShapeA bool
	// ExclusiveB links each B to at most one A: the best-scoring one. An
	// evacuation zone belongs to the nearest fire, not every fire nearby.
	ExclusiveB bool
	// Judge decides a pair that passed the time and space bounds.
	Judge func(p Pair) (Match, bool)
}

// Pair is what Judge sees: the two events and facts about them.
type Pair struct {
	A, B *gridv1.Event
	// GapKm is the distance between their bounding boxes (0 when they meet).
	GapKm float64
	// AShape is A's parsed full geometry when Rule.ShapeA, else nil.
	AShape *geojson.Geom
	// Lag is B's start minus A's start.
	Lag time.Duration
}

// Relation is one related event, from the subject's point of view.
type Relation struct {
	Event      *gridv1.Event
	Rule       string
	Confidence Confidence
	Reason     string
}

// Store is the read side the engine needs (satisfied by *store.Store).
type Store interface {
	GetEvent(ctx context.Context, id string) (*gridv1.Event, error)
	QueryEventsPage(ctx context.Context, q store.EventQuery) (store.Page[*gridv1.Event], error)
	FirstIngested(ctx context.Context, ids []string) (map[string]time.Time, error)
}

// Engine applies Rules. Create one per request: it is not safe for
// concurrent use (it caches first-stored times while it works).
type Engine struct {
	Store Store
	Rules []Rule
	Now   func() time.Time
	// MaxCandidates bounds the events read per rule side (newest first).
	MaxCandidates int

	// firstSeen is when the Grid first stored events that state no start of
	// their own (Cal OES zones, FIRIS perimeters); see span.
	firstSeen map[string]time.Time
}

// New returns an engine over the default rules.
func New(st Store) *Engine {
	return &Engine{Store: st, Rules: Rules(), Now: time.Now, MaxCandidates: 2000}
}

// Related returns the events related to subject, strongest first. A pair
// matched by several rules appears once, under its strongest rule.
func (e *Engine) Related(ctx context.Context, subject *gridv1.Event) ([]Relation, error) {
	e.firstSeen = map[string]time.Time{}
	if err := e.noteFirstSeen(ctx, []*gridv1.Event{subject}); err != nil {
		return nil, err
	}
	best := map[string]Relation{}
	keep := func(r Relation) {
		if cur, ok := best[r.Event.GetId()]; !ok || r.Confidence > cur.Confidence {
			best[r.Event.GetId()] = r
		}
	}
	for _, rule := range e.Rules {
		if rule.A.has(subject) {
			ms, err := e.fromA(ctx, rule, subject)
			if err != nil {
				return nil, err
			}
			for _, m := range ms {
				keep(m)
			}
		}
		if rule.B.has(subject) {
			ms, err := e.fromB(ctx, rule, subject)
			if err != nil {
				return nil, err
			}
			for _, m := range ms {
				keep(m)
			}
		}
	}
	out := make([]Relation, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Event.GetId() < out[j].Event.GetId()
	})
	return out, nil
}

// fromA: subject is an A; find Bs.
func (e *Engine) fromA(ctx context.Context, rule Rule, a *gridv1.Event) ([]Relation, error) {
	aStart, aEnd := e.span(a)
	cands, err := e.candidates(ctx, rule.B, aStart.Add(-rule.Before), aEnd.Add(rule.After))
	if err != nil {
		return nil, err
	}
	shape, err := e.shape(ctx, rule, a)
	if err != nil {
		return nil, err
	}
	var out []Relation
	for _, b := range cands {
		m, ok := e.judge(rule, a, b, shape)
		if !ok {
			continue
		}
		if rule.ExclusiveB {
			// Keep it only if no other A suits this B better.
			bestA, err := e.bestA(ctx, rule, b)
			if err != nil {
				return nil, err
			}
			if bestA != a.GetId() {
				continue
			}
		}
		out = append(out, Relation{Event: b, Rule: rule.Name, Confidence: m.Confidence, Reason: m.Reason})
	}
	return out, nil
}

// fromB: subject is a B; find As.
func (e *Engine) fromB(ctx context.Context, rule Rule, b *gridv1.Event) ([]Relation, error) {
	matches, err := e.asFor(ctx, rule, b)
	if err != nil {
		return nil, err
	}
	if rule.ExclusiveB && len(matches) > 1 {
		matches = matches[:1]
	}
	out := make([]Relation, 0, len(matches))
	for _, m := range matches {
		out = append(out, Relation{Event: m.a, Rule: rule.Name, Confidence: m.Confidence, Reason: m.Reason})
	}
	return out, nil
}

type aMatch struct {
	a *gridv1.Event
	Match
}

// asFor lists the As that match b, best (lowest score) first.
func (e *Engine) asFor(ctx context.Context, rule Rule, b *gridv1.Event) ([]aMatch, error) {
	bStart, _ := e.span(b)
	// A's span must reach B's start window: A.start <= bStart+Before and
	// A.end >= bStart-After. Candidates are read by A's last update.
	cands, err := e.candidates(ctx, rule.A, bStart.Add(-rule.After), bStart.Add(rule.Before))
	if err != nil {
		return nil, err
	}
	var out []aMatch
	for _, a := range cands {
		shape, err := e.shape(ctx, rule, a)
		if err != nil {
			return nil, err
		}
		if m, ok := e.judge(rule, a, b, shape); ok {
			out = append(out, aMatch{a: a, Match: m})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Score < out[j].Score
	})
	return out, nil
}

func (e *Engine) bestA(ctx context.Context, rule Rule, b *gridv1.Event) (string, error) {
	as, err := e.asFor(ctx, rule, b)
	if err != nil || len(as) == 0 {
		return "", err
	}
	return as[0].a.GetId(), nil
}

// judge applies the rule's bounds, then its Judge.
func (e *Engine) judge(rule Rule, a, b *gridv1.Event, shape *geojson.Geom) (Match, bool) {
	if a.GetId() == b.GetId() {
		return Match{}, false
	}
	aStart, aEnd := e.span(a)
	bStart, _ := e.span(b)
	if bStart.Before(aStart.Add(-rule.Before)) || bStart.After(aEnd.Add(rule.After)) {
		return Match{}, false
	}
	gap := bboxGapKm(bboxOf(a), bboxOf(b))
	if rule.MaxKm > 0 && gap > rule.MaxKm {
		return Match{}, false
	}
	return rule.Judge(Pair{A: a, B: b, GapKm: gap, AShape: shape, Lag: bStart.Sub(aStart)})
}

// span is when an event began and ended (or now, while it is live). Start
// is the source's `effective` when given, else when the Grid first stored
// it, else its last update; end is the source's expiry when that is earlier,
// else now for a live event, else the transition that ended it. Without the
// first-stored fallback, a Cal OES zone (no effective) "started" at its last
// update, which for an ended zone is when it ended, days after its fire.
func (e *Engine) span(ev *gridv1.Event) (time.Time, time.Time) {
	now := e.Now()
	observed := ev.GetObservedAt().AsTime()
	start := observed
	if eff := ev.GetEffective(); eff != nil && !eff.AsTime().IsZero() {
		start = eff.AsTime()
	} else if first, ok := e.firstSeen[ev.GetId()]; ok && first.Before(start) {
		start = first
	}
	end := now
	switch ev.GetStatus() {
	case gridv1.EventStatus_RESOLVED, gridv1.EventStatus_EXPIRED:
		end = observed
	}
	if x := ev.GetExpires(); x != nil && x.AsTime().Before(end) && x.AsTime().After(start) {
		end = x.AsTime()
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

// candidates reads one side's events that were live at some point at or
// after from and began by to, without shapes. A live event qualifies however
// long ago it was last updated; an ended one must have ended at or after
// from (its last update is the transition that ended it).
func (e *Engine) candidates(ctx context.Context, side Side, from, to time.Time) ([]*gridv1.Event, error) {
	var out []*gridv1.Event
	for _, q := range []store.EventQuery{
		{Statuses: []gridv1.EventStatus{gridv1.EventStatus_ACTIVE, gridv1.EventStatus_SCHEDULED}},
		{Statuses: []gridv1.EventStatus{gridv1.EventStatus_RESOLVED, gridv1.EventStatus_EXPIRED}, Since: from},
	} {
		q.Layers, q.PageSize, q.Lite = []gridv1.Layer{side.Layer}, store.MaxPageSize, true
		for {
			page, err := e.Store.QueryEventsPage(ctx, q)
			if err != nil {
				return nil, err
			}
			if err := e.noteFirstSeen(ctx, page.Items); err != nil {
				return nil, err
			}
			for _, ev := range page.Items {
				if s, _ := e.span(ev); s.After(to) || !side.has(ev) {
					continue
				}
				out = append(out, ev)
			}
			q.PageToken = page.Next(len(page.Items))
			if q.PageToken == "" || len(out) >= e.MaxCandidates {
				break
			}
		}
	}
	return out, nil
}

func (e *Engine) shape(ctx context.Context, rule Rule, a *gridv1.Event) (*geojson.Geom, error) {
	if !rule.ShapeA {
		return nil, nil
	}
	raw := a.GetGeometry().GetGeojson()
	if len(raw) == 0 {
		full, err := e.Store.GetEvent(ctx, a.GetId())
		if err != nil {
			return nil, err
		}
		raw = full.GetGeometry().GetGeojson()
	}
	if len(raw) == 0 {
		return nil, nil
	}
	g, err := geojson.Parse(raw)
	if err != nil {
		return nil, nil // an unreadable shape means no containment, not an error
	}
	return g, nil
}

// bboxGapKm is the distance between two bounding boxes, 0 when they touch,
// +Inf when either is missing.
func bboxGapKm(a, b *gridv1.BoundingBox) float64 {
	if a == nil || b == nil {
		return math.Inf(1)
	}
	dLat := math.Max(0, math.Max(a.GetMinLat()-b.GetMaxLat(), b.GetMinLat()-a.GetMaxLat()))
	dLng := math.Max(0, math.Max(a.GetMinLng()-b.GetMaxLng(), b.GetMinLng()-a.GetMaxLng()))
	midLat := (a.GetMinLat() + a.GetMaxLat() + b.GetMinLat() + b.GetMaxLat()) / 4
	kx := math.Cos(midLat * math.Pi / 180)
	return math.Hypot(dLat*111.32, dLng*111.32*kx)
}

// bboxOf is the event's bounding box, derived from its shape when the stored
// box is absent (an event not yet written by the store).
func bboxOf(ev *gridv1.Event) *gridv1.BoundingBox {
	g := ev.GetGeometry()
	if b := g.GetBbox(); b != nil {
		return b
	}
	if len(g.GetGeojson()) == 0 {
		return nil
	}
	parsed, err := geojson.Parse(g.GetGeojson())
	if err != nil {
		return nil
	}
	minLat, minLng, maxLat, maxLng := parsed.Bbox()
	return &gridv1.BoundingBox{MinLat: minLat, MinLng: minLng, MaxLat: maxLat, MaxLng: maxLng}
}

// centroidOf is the event's centroid, derived from its shape when absent.
func centroidOf(ev *gridv1.Event) *gridv1.LatLng {
	g := ev.GetGeometry()
	if c := g.GetCentroid(); c != nil {
		return c
	}
	if len(g.GetGeojson()) == 0 {
		return nil
	}
	parsed, err := geojson.Parse(g.GetGeojson())
	if err != nil {
		return nil
	}
	lat, lng := parsed.Centroid()
	return &gridv1.LatLng{Lat: lat, Lng: lng}
}

// noteFirstSeen records first-stored times for events with no effective.
func (e *Engine) noteFirstSeen(ctx context.Context, evs []*gridv1.Event) error {
	if e.firstSeen == nil {
		e.firstSeen = map[string]time.Time{}
	}
	var ids []string
	for _, ev := range evs {
		if ev.GetEffective() == nil {
			if _, done := e.firstSeen[ev.GetId()]; !done {
				ids = append(ids, ev.GetId())
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	got, err := e.Store.FirstIngested(ctx, ids)
	if err != nil {
		return err
	}
	for id, at := range got {
		e.firstSeen[id] = at
	}
	return nil
}
