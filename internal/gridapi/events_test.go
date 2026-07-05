package gridapi

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture recap (seedEvents): default statuses (ACTIVE+SCHEDULED) yield, in
// canonical order (severity desc, observed_at desc, id asc):
//
//	calfire:f1 (SEVERE, base), usgs:q1 (MODERATE, base+3h), wx:a1 (MODERATE, base-1h)
//
// chp:i1 is RESOLVED and excluded by default.

func TestEventsDefaultStatuses(t *testing.T) {
	s := newTestService(t)
	rec := get(t, s, "/v1/events")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	ids, next := eventIDs(t, rec)
	assert.Equal(t, []string{"calfire:f1", "usgs:q1", "wx:a1"}, ids,
		"RESOLVED excluded by default; canonical sort")
	assert.Empty(t, next)
}

func TestEventsFilters(t *testing.T) {
	s := newTestService(t)

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"layer slug lowercase", "layer=wildfire", []string{"calfire:f1"}},
		{"layer enum name", "layer=EARTHQUAKE", []string{"usgs:q1"}},
		{"layer mixed case", "layer=WeAtHeR_aLeRt", []string{"wx:a1"}},
		{"layer repeated", "layer=wildfire&layer=earthquake", []string{"calfire:f1", "usgs:q1"}},
		{"layer comma list", "layer=wildfire,earthquake", []string{"calfire:f1", "usgs:q1"}},
		{"status explicit", "status=resolved", []string{"chp:i1"}},
		{"status repeated", "status=ACTIVE&status=RESOLVED", []string{"calfire:f1", "usgs:q1", "chp:i1"}},
		{"severity_min", "severity_min=severe", []string{"calfire:f1"}},
		{"severity_min INFO is no-op", "severity_min=INFO", []string{"calfire:f1", "usgs:q1", "wx:a1"}},
		{"since", "since=" + url.QueryEscape(base.Add(time.Hour).Format(time.RFC3339)), []string{"usgs:q1"}},
		{"place slug", "place=calaveras", []string{"calfire:f1", "usgs:q1", "wx:a1"}},
		{"place id form", "place=" + url.QueryEscape("area:calaveras"), []string{"calfire:f1", "usgs:q1", "wx:a1"}},
		{"place scopes spatially", "place=high-country", []string{"usgs:q1"}},
		{"combination", "place=calaveras&layer=wildfire,earthquake&severity_min=SEVERE", []string{"calfire:f1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, s, "/v1/events?"+tc.query)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			ids, _ := eventIDs(t, rec)
			assert.Equal(t, tc.want, ids)
		})
	}
}

func TestEventsBadInputs(t *testing.T) {
	s := newTestService(t)

	badRequests := []string{
		"/v1/events?layer=bogus",
		"/v1/events?layer=LAYER_UNSPECIFIED",
		"/v1/events?status=bogus",
		"/v1/events?status=EVENT_STATUS_UNSPECIFIED",
		"/v1/events?severity_min=mega",
		"/v1/events?since=yesterday",
		"/v1/events?page_size=abc",
		"/v1/events?page_size=0",
		"/v1/events?page_size=201",
		"/v1/events?page_token=%25%25not-base64",
		"/v1/events?page_token=bm90LWpzb24", // valid base64, invalid cursor JSON
	}
	for _, path := range badRequests {
		rec := get(t, s, path)
		requireStatus(t, rec, http.StatusBadRequest, 3)
	}

	rec := get(t, s, "/v1/events?place=atlantis")
	requireStatus(t, rec, http.StatusNotFound, 5)
}

// Two-page walk: keyset pagination keeps the canonical order stable and
// complete across pages.
func TestEventsPagination(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/events?page_size=2")
	require.Equal(t, http.StatusOK, rec.Code)
	ids, token := eventIDs(t, rec)
	require.Equal(t, []string{"calfire:f1", "usgs:q1"}, ids)
	require.NotEmpty(t, token)

	rec = get(t, s, "/v1/events?page_size=2&page_token="+url.QueryEscape(token))
	require.Equal(t, http.StatusOK, rec.Code)
	ids, token = eventIDs(t, rec)
	assert.Equal(t, []string{"wx:a1"}, ids)
	assert.Empty(t, token, "last page")
}

func TestEventByID(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/events/usgs:q1")
	require.Equal(t, http.StatusOK, rec.Code)
	var ev struct {
		ID       string   `json:"id"`
		Layer    string   `json:"layer"`
		Severity string   `json:"severity"`
		Headline string   `json:"headline"`
		Revision uint32   `json:"revision"`
		PlaceIDs []string `json:"place_ids"`
	}
	decode(t, rec, &ev)
	assert.Equal(t, "usgs:q1", ev.ID)
	assert.Equal(t, "EARTHQUAKE", ev.Layer, "enums render as proto names")
	assert.Equal(t, "MODERATE", ev.Severity)
	assert.Equal(t, "M4.4 earthquake near Arnold (revised)", ev.Headline, "current revision")
	assert.Equal(t, uint32(2), ev.Revision)
	assert.Contains(t, ev.PlaceIDs, "area:calaveras", "geometric place attachment on the wire")

	rec = get(t, s, "/v1/events/usgs:nope")
	requireStatus(t, rec, http.StatusNotFound, 5)
}

// revisionList decodes an EventRevisionList body.
type revisionList struct {
	Revisions []struct {
		Revision uint32 `json:"revision"`
		Event    struct {
			ID       string `json:"id"`
			Headline string `json:"headline"`
		} `json:"event"`
		ObservedAt string `json:"observed_at"`
	} `json:"revisions"`
	NextPageToken string `json:"next_page_token"`
}

func TestEventHistory(t *testing.T) {
	s := newTestService(t)

	rec := get(t, s, "/v1/events/usgs:q1/history")
	require.Equal(t, http.StatusOK, rec.Code)
	var out revisionList
	decode(t, rec, &out)
	require.Len(t, out.Revisions, 2)
	assert.Equal(t, uint32(2), out.Revisions[0].Revision, "newest first")
	assert.Equal(t, uint32(1), out.Revisions[1].Revision)
	assert.Equal(t, "M4.4 earthquake near Arnold (revised)", out.Revisions[0].Event.Headline)
	assert.Equal(t, "M4.2 earthquake near Arnold", out.Revisions[1].Event.Headline)
	assert.Empty(t, out.NextPageToken)

	t.Run("paginated", func(t *testing.T) {
		rec := get(t, s, "/v1/events/usgs:q1/history?page_size=1")
		require.Equal(t, http.StatusOK, rec.Code)
		var page1 revisionList
		decode(t, rec, &page1)
		require.Len(t, page1.Revisions, 1)
		assert.Equal(t, uint32(2), page1.Revisions[0].Revision)
		require.NotEmpty(t, page1.NextPageToken)

		rec = get(t, s, "/v1/events/usgs:q1/history?page_size=1&page_token="+url.QueryEscape(page1.NextPageToken))
		require.Equal(t, http.StatusOK, rec.Code)
		var page2 revisionList
		decode(t, rec, &page2)
		require.Len(t, page2.Revisions, 1)
		assert.Equal(t, uint32(1), page2.Revisions[0].Revision)
		assert.Empty(t, page2.NextPageToken)
	})

	t.Run("unknown event 404", func(t *testing.T) {
		rec := get(t, s, "/v1/events/usgs:nope/history")
		requireStatus(t, rec, http.StatusNotFound, 5)
	})
	t.Run("bad token 400", func(t *testing.T) {
		rec := get(t, s, "/v1/events/usgs:q1/history?page_token=%25bad")
		requireStatus(t, rec, http.StatusBadRequest, 3)
	})
}

func TestHistory(t *testing.T) {
	s := newTestService(t)

	// All five fixture revisions, observed_at desc: q1r2 (base+3h), q1r1
	// (base+2h), f1 (base), a1 (base-1h), i1 (base-2h).
	rec := get(t, s, "/v1/history")
	require.Equal(t, http.StatusOK, rec.Code)
	var out revisionList
	decode(t, rec, &out)
	require.Len(t, out.Revisions, 5)
	assert.Equal(t, "usgs:q1", out.Revisions[0].Event.ID)
	assert.Equal(t, uint32(2), out.Revisions[0].Revision)
	assert.Equal(t, "chp:i1", out.Revisions[4].Event.ID)

	t.Run("from bound", func(t *testing.T) {
		from := url.QueryEscape(base.Add(150 * time.Minute).Format(time.RFC3339))
		rec := get(t, s, "/v1/history?from="+from)
		require.Equal(t, http.StatusOK, rec.Code)
		var out revisionList
		decode(t, rec, &out)
		require.Len(t, out.Revisions, 1)
		assert.Equal(t, uint32(2), out.Revisions[0].Revision)
	})
	t.Run("to bound exclusive", func(t *testing.T) {
		to := url.QueryEscape(base.Format(time.RFC3339))
		rec := get(t, s, "/v1/history?to="+to)
		require.Equal(t, http.StatusOK, rec.Code)
		var out revisionList
		decode(t, rec, &out)
		// observed_at < base: a1 and i1 only (f1 at exactly base is excluded).
		require.Len(t, out.Revisions, 2)
		assert.Equal(t, "wx:a1", out.Revisions[0].Event.ID)
		assert.Equal(t, "chp:i1", out.Revisions[1].Event.ID)
	})
	t.Run("layer filter", func(t *testing.T) {
		rec := get(t, s, "/v1/history?layer=wildfire")
		require.Equal(t, http.StatusOK, rec.Code)
		var out revisionList
		decode(t, rec, &out)
		require.Len(t, out.Revisions, 1)
		assert.Equal(t, "calfire:f1", out.Revisions[0].Event.ID)
	})
	t.Run("place filter", func(t *testing.T) {
		rec := get(t, s, "/v1/history?place=high-country")
		require.Equal(t, http.StatusOK, rec.Code)
		var out revisionList
		decode(t, rec, &out)
		require.Len(t, out.Revisions, 2, "only the quake sits in high-country")
		assert.Equal(t, "usgs:q1", out.Revisions[0].Event.ID)
		assert.Equal(t, "usgs:q1", out.Revisions[1].Event.ID)
	})
	t.Run("bad inputs", func(t *testing.T) {
		requireStatus(t, get(t, s, "/v1/history?from=notatime"), http.StatusBadRequest, 3)
		requireStatus(t, get(t, s, "/v1/history?to=notatime"), http.StatusBadRequest, 3)
		requireStatus(t, get(t, s, "/v1/history?layer=bogus"), http.StatusBadRequest, 3)
		requireStatus(t, get(t, s, "/v1/history?place=atlantis"), http.StatusNotFound, 5)
	})
	t.Run("pagination", func(t *testing.T) {
		rec := get(t, s, "/v1/history?page_size=3")
		require.Equal(t, http.StatusOK, rec.Code)
		var page1 revisionList
		decode(t, rec, &page1)
		require.Len(t, page1.Revisions, 3)
		require.NotEmpty(t, page1.NextPageToken)

		rec = get(t, s, "/v1/history?page_size=3&page_token="+url.QueryEscape(page1.NextPageToken))
		require.Equal(t, http.StatusOK, rec.Code)
		var page2 revisionList
		decode(t, rec, &page2)
		require.Len(t, page2.Revisions, 2)
		assert.Empty(t, page2.NextPageToken)
	})
}
