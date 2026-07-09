package gridapi

import (
	"fmt"
	"strings"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
)

// parseKind maps a place-kind param (enum name, case-insensitive, so
// lowercase "town"/"evac_zone" work) onto the enum; unknown or UNSPECIFIED
// values are rejected — omit the param to mean "all kinds".
func parseKind(v string) (gridv1.PlaceKind, error) {
	n, ok := gridv1.PlaceKind_value[strings.ToUpper(strings.TrimSpace(v))]
	if !ok || n == int32(gridv1.PlaceKind_PLACE_KIND_UNSPECIFIED) {
		return 0, fmt.Errorf("unknown kind: %q", v)
	}
	return gridv1.PlaceKind(n), nil
}
