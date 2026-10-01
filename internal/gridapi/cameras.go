package gridapi

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/dpup/prefab/plugins/etag"

	gridv1 "github.com/dpup/sierra-data/api/grid/v1"
	"github.com/dpup/sierra-data/internal/clients/cwwp2"
	"github.com/dpup/sierra-data/internal/lib/geojson"
	"github.com/dpup/sierra-data/internal/store"
)

// cameraAttribution credits the camera images, which Caltrans serves.
const cameraAttribution = "Caltrans"

// CameraDirectory is the slice of services.CameraService ListCameras consumes:
// the served camera list, its OK/STALE/UNAVAILABLE status, when a non-OK list
// was fetched, and a version that changes with any of them (an ETag input).
type CameraDirectory interface {
	Cameras() (cams []cwwp2.Camera, status string, lastSourceUpdate time.Time, version string)
}

// ListCameras serves the Caltrans CCTV directory. Two radius tests, both at
// roads.caltransFeeds.cwwp2.cameras.nearMeters:
//
//   - the GEOGRAPHY: a camera is listed only within that distance of a
//     coverage AREA polygon. The districts we fetch reach far past the
//     footprint (District 10 is mostly Valley freeway cameras);
//   - ?place: of those, keep the ones within that distance of the place.
//     Containment alone would be useless here — Calaveras County, every
//     corridor and every town (a point) contain no camera at all — and a
//     camera on the approach road is the view that answers "is the pass open".
//
// distanceMeters is to the place, or unscoped to the nearest coverage area, and
// orders the list nearest first.
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

	areas, err := g.svc.coverageGeoms(ctx)
	if err != nil {
		return nil, internalErr(ctx, err)
	}
	var placeGeom *geojson.Geom
	if place != nil {
		// A place whose geometry won't parse is unlocatable, and unlocatable is
		// not "everywhere": it gets no cameras.
		placeGeom, _ = geojson.Parse(place.GetGeometry().GetGeojson())
	}
	near := g.svc.Cfg.Roads.CaltransFeeds.CWWP2.Cameras.Near()

	type ranked struct {
		cam  *gridv1.Camera
		dist float64
	}
	var keep []ranked
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
		keep = append(keep, ranked{cam: projectCamera(c, dist), dist: dist})
	}
	sort.SliceStable(keep, func(i, j int) bool {
		if keep[i].dist != keep[j].dist {
			return keep[i].dist < keep[j].dist
		}
		return keep[i].cam.GetId() < keep[j].cam.GetId()
	})

	out := &gridv1.CameraList{
		Cameras:          make([]*gridv1.Camera, 0, len(keep)),
		SourceStatus:     status,
		LastSourceUpdate: tsOrNil(lastUpdate),
		Attribution:      cameraAttribution,
	}
	for _, k := range keep {
		out.Cameras = append(out.Cameras, k.cam)
	}
	return out, nil
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
