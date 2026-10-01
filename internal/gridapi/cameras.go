package gridapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dpup/prefab/plugins/etag"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/hazards"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"github.com/dpup/sierra-data/internal/store"
)

// cameraAttribution credits the camera images, which Caltrans serves.
const cameraAttribution = "Caltrans"

// CameraDirectory is the slice of services.CameraService the camera RPC and
// map layer consume: the served camera list, its OK/STALE/UNAVAILABLE status,
// when a non-OK list was fetched, and a version that changes with any of them
// (an ETag input).
type CameraDirectory interface {
	Cameras() (cams []cwwp2.Camera, status string, lastSourceUpdate time.Time, version string)
}

// rankedCamera is a camera that passed camerasNear, with its distance.
type rankedCamera struct {
	cam  cwwp2.Camera
	dist float64
}

// camerasNear applies the two radius tests the camera RPC and map layer share,
// both at roads.caltransFeeds.cwwp2.cameras.nearMeters:
//
//   - the GEOGRAPHY: a camera is kept only within that distance of a coverage
//     AREA polygon. The districts we fetch reach far past the footprint
//     (District 10 is mostly Valley freeway cameras);
//   - the PLACE (when non-nil): of those, keep the ones within that distance
//     of it. Containment alone would be useless here — Calaveras County, every
//     corridor and every town (a point) contain no camera at all — and a
//     camera on the approach road is the view that answers "is the pass open".
//
// The distance is to the place, or without one to the nearest coverage area,
// and orders the result nearest first, then by id.
func (s *Service) camerasNear(ctx context.Context, cams []cwwp2.Camera, place *gridv1.Place) ([]rankedCamera, error) {
	areas, err := s.coverageGeoms(ctx)
	if err != nil {
		return nil, err
	}
	var placeGeom *geojson.Geom
	if place != nil {
		// A place whose geometry won't parse is unlocatable, and unlocatable is
		// not "everywhere": it gets no cameras.
		placeGeom, _ = geojson.Parse(place.GetGeometry().GetGeojson())
	}
	near := s.Cfg.Roads.CaltransFeeds.CWWP2.Cameras.Near()

	var keep []rankedCamera
	for _, c := range cams {
		lat, lng := c.Location.Latitude, c.Location.Longitude
		dist := math.Inf(1)
		for _, a := range areas {
			dist = math.Min(dist, geojson.PointDistanceMeters(lat, lng, a))
		}
		if dist > near {
			continue
		}
		if place != nil {
			if dist = geojson.PointDistanceMeters(lat, lng, placeGeom); dist > near {
				continue
			}
		}
		keep = append(keep, rankedCamera{cam: c, dist: dist})
	}
	sort.SliceStable(keep, func(i, j int) bool {
		if keep[i].dist != keep[j].dist {
			return keep[i].dist < keep[j].dist
		}
		return keep[i].cam.ID < keep[j].cam.ID
	})
	return keep, nil
}

// ListCameras serves the Caltrans CCTV directory: camerasNear over the whole
// coverage area, or around ?place. The map layer (serveCameraLayer) serves the
// same cameras for a place as GeoJSON.
func (g *GridServer) ListCameras(ctx context.Context, req *gridv1.ListCamerasRequest) (*gridv1.CameraList, error) {
	var place *gridv1.Place
	if p := req.GetPlace(); p != "" {
		var err error
		place, err = g.svc.Store.GetPlace(ctx, p)
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFoundErr("unknown place: %q", p)
		}
		if err != nil {
			return nil, internalErr(ctx, err)
		}
	}
	if g.svc.Cameras == nil {
		// Not configured (no camera districts): say so rather than serve an
		// empty list that reads as "no cameras near here".
		return &gridv1.CameraList{SourceStatus: "UNAVAILABLE", Attribution: cameraAttribution}, nil
	}

	cams, status, lastUpdate, version := g.svc.Cameras.Cameras()
	// The list changes only on a refresh (version) and the geography only on a
	// redeploy (placesVersion covers the seeded areas and the radius config).
	if err := etag.Guard(ctx, weakListTag(g.placesVersion+"."+version, req.GetPlace())); err != nil {
		return nil, err // 304 — skip the distance pass
	}
	ranked, err := g.svc.camerasNear(ctx, cams, place)
	if err != nil {
		return nil, internalErr(ctx, err)
	}

	out := &gridv1.CameraList{
		Cameras:          make([]*gridv1.Camera, 0, len(ranked)),
		SourceStatus:     status,
		LastSourceUpdate: tsOrNil(lastUpdate),
		Attribution:      cameraAttribution,
	}
	for _, rc := range ranked {
		out.Cameras = append(out.Cameras, projectCamera(rc.cam, rc.dist))
	}
	return out, nil
}

// serveCameraLayer handles GET /api/v1/places/{place}/map/camera.geojson: the
// cameras ListCameras?place= returns, one INFO Point each. Reference views, not
// hazards — the layer never feeds the place summary. sourceStatus is the camera
// LIST's health, exactly as on the RPC; an unconfigured directory is
// UNAVAILABLE, never an OK-empty that reads as "no cameras near here".
func (s *Service) serveCameraLayer(w http.ResponseWriter, r *http.Request, place *gridv1.Place) {
	ctx := r.Context()
	md := &hazards.Metadata{
		Layer:         hazards.LayerCamera,
		Area:          place.GetSlug(),
		GeneratedAt:   s.Now().UTC().Format(time.RFC3339),
		SourceStatus:  "UNAVAILABLE",
		Attribution:   cameraAttribution,
		SchemaVersion: mapSchemaVersion,
	}
	if s.Cameras == nil {
		s.writeFeatureCollection(w, r, nil, md)
		return
	}
	cams, status, lastUpdate, _ := s.Cameras.Cameras()
	ranked, err := s.camerasNear(ctx, cams, place)
	if err != nil {
		internal(ctx, w, err)
		return
	}
	features := make([]hazards.Feature, 0, len(ranked))
	for _, rc := range ranked {
		features = append(features, cameraFeature(rc.cam, rc.dist))
	}
	md.SourceStatus = status
	md.LastSourceUpdate = timeOrEmpty(lastUpdate)
	s.writeFeatureCollection(w, r, features, md)
}

// coverageGeoms parses the coverage AREA places' geometries — the footprint
// the camera geography is measured from. A geometry that won't parse is
// skipped (the seed validates them, so this is belt and braces).
func (s *Service) coverageGeoms(ctx context.Context) ([]*geojson.Geom, error) {
	areas, err := s.Store.ListPlaces(ctx, gridv1.PlaceKind_AREA, "")
	if err != nil {
		return nil, err
	}
	out := make([]*geojson.Geom, 0, len(areas))
	for _, a := range areas {
		if g, err := geojson.Parse(a.GetGeometry().GetGeojson()); err == nil {
			out = append(out, g)
		}
	}
	return out, nil
}

// projectCamera maps a client camera onto the wire message.
func projectCamera(c cwwp2.Camera, dist float64) *gridv1.Camera {
	return &gridv1.Camera{
		Id:                  c.ID,
		Name:                c.Name,
		NearbyPlace:         c.Location.NearbyPlace,
		County:              c.Location.County,
		Route:               c.Location.Route,
		Direction:           c.Location.Direction,
		Location:            &gridv1.LatLng{Lat: c.Location.Latitude, Lng: c.Location.Longitude},
		ElevationFeet:       int32(math.Round(c.Location.ElevationFt)),
		ImageUrl:            c.ImageURL,
		ImageRefreshMinutes: int32(c.ImageRefreshMinutes),
		StreamUrl:           c.StreamURL,
		Description:         c.Description,
		DistanceMeters:      int32(math.Round(dist)),
	}
}

// cameraFeature maps a client camera onto a map-layer Point. The feature id is
// the camera id ListCameras uses, so a client can join the two. source.url is
// the live snapshot, which is the camera's own page as far as a reader cares.
func cameraFeature(c cwwp2.Camera, dist float64) hazards.Feature {
	return hazards.Feature{
		Type:     "Feature",
		Geometry: hazards.PointGeom(c.Location.Latitude, c.Location.Longitude),
		Properties: hazards.Properties{
			ID:           c.ID,
			Layer:        strings.ToUpper(hazards.LayerCamera),
			Kind:         "Traffic camera",
			Severity:     gridv1.Severity_INFO.String(),
			SeverityRank: int(gridv1.Severity_INFO.Number()),
			Headline:     c.Name,
			Description:  c.Description,
			AreaLabel:    c.Location.NearbyPlace,
			Source:       hazards.Source{ID: "caltrans", Name: "Caltrans CCTV", URL: c.ImageURL, Attribution: cameraAttribution},
			Camera: &hazards.CameraProps{
				ImageURL:            c.ImageURL,
				ImageRefreshMinutes: int32(c.ImageRefreshMinutes),
				StreamURL:           c.StreamURL,
				Route:               c.Location.Route,
				Direction:           c.Location.Direction,
				County:              c.Location.County,
				ElevationFeet:       int32(math.Round(c.Location.ElevationFt)),
				DistanceMeters:      int32(math.Round(dist)),
			},
		},
	}
}
