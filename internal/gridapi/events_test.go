package gridapi

import (
	"testing"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLayers(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []gridv1.Layer
		err  bool
	}{
		{name: "enum name", in: []string{"WILDFIRE"}, want: []gridv1.Layer{gridv1.Layer_WILDFIRE}},
		{name: "lowercase slug", in: []string{"road_incident"}, want: []gridv1.Layer{gridv1.Layer_ROAD_INCIDENT}},
		{name: "comma list", in: []string{"wildfire,earthquake"}, want: []gridv1.Layer{gridv1.Layer_WILDFIRE, gridv1.Layer_EARTHQUAKE}},
		// The mesh-node layer's enum is MESH; "mesh_node" (map slug) and the legacy
		// "network" resolve to it as aliases (case-insensitive).
		{name: "mesh enum name", in: []string{"mesh"}, want: []gridv1.Layer{gridv1.Layer_MESH}},
		{name: "network legacy alias", in: []string{"network"}, want: []gridv1.Layer{gridv1.Layer_MESH}},
		{name: "mesh_node slug alias", in: []string{"mesh_node"}, want: []gridv1.Layer{gridv1.Layer_MESH}},
		{name: "mesh uppercase", in: []string{"MESH"}, want: []gridv1.Layer{gridv1.Layer_MESH}},
		{name: "unknown", in: []string{"volcano"}, err: true},
		{name: "unspecified rejected", in: []string{"LAYER_UNSPECIFIED"}, err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLayers(tc.in)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
