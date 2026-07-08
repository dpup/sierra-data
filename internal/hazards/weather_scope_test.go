package hazards

import (
	"context"
	"testing"

	api "github.com/dpup/sierra-data/api/v1"
	"github.com/dpup/sierra-data/internal/config"
)

func TestZonesMatch(t *testing.T) {
	cases := []struct {
		name       string
		area, alrt []string
		want       bool
	}{
		{"unscoped area keeps everything", nil, []string{"CAZ258"}, true},
		{"zoneless alert (OWM) kept", []string{"CAZ064"}, nil, true},
		{"intersecting zones match", []string{"CAZ064", "CAZ065"}, []string{"CAZ065"}, true},
		{"disjoint zones drop", []string{"CAZ064", "CAZ065"}, []string{"CAZ258"}, false},
	}
	for _, c := range cases {
		if got := zonesMatch(c.area, c.alrt); got != c.want {
			t.Errorf("%s: zonesMatch(%v,%v)=%v want %v", c.name, c.area, c.alrt, got, c.want)
		}
	}
}

// fakeWeather implements WeatherAPI for builder tests.
type fakeWeather struct {
	fw *api.FireWeather
}

func (f fakeWeather) ListWeather(context.Context, *api.ListWeatherRequest) (*api.ListWeatherResponse, error) {
	return &api.ListWeatherResponse{FireWeather: f.fw}, nil
}

// TestFireWeather_AreaScoping: fire weather only surfaces for areas whose zones
// the product covers.
func TestFireWeather_AreaScoping(t *testing.T) {
	fw := &api.FireWeather{State: api.FireWeatherState_RED_FLAG, Zones: []string{"CAZ258", "CAZ259"}}
	s := &Service{weather: fakeWeather{fw: fw}}

	// Tuolumne area: covered → one feature.
	in, _ := s.fireWeather(context.Background(), config.HazardArea{Zones: []string{"CAZ258"}})
	if len(in) != 1 {
		t.Fatalf("covered area got %d fire-weather features, want 1", len(in))
	}
	// Calaveras-only area: not covered → none.
	out, _ := s.fireWeather(context.Background(), config.HazardArea{Zones: []string{"CAZ064"}})
	if len(out) != 0 {
		t.Errorf("uncovered area got %d fire-weather features, want 0", len(out))
	}
}
