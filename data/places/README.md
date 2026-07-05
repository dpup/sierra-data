# Place directory seed data

## counties.geojson

Generalized (1:500,000 cartographic-boundary) county polygons for the eight
counties in and around the service area: Alpine, Amador, Calaveras, El Dorado,
Mariposa, San Joaquin, Stanislaus, and Tuolumne (all California, GEOID prefix
`06`).

- **Source**: U.S. Census Bureau TIGERweb ArcGIS REST,
  `Generalized_ACS2023/State_County` MapServer, layer 11 ("Counties 500K",
  January 1, 2023 vintage).
- **Fetch URL**:
  `https://tigerweb.geo.census.gov/arcgis/rest/services/Generalized_ACS2023/State_County/MapServer/11/query`
  with parameters
  `where=STATE='06' AND BASENAME IN ('Calaveras','Tuolumne','Amador','Alpine','Stanislaus','San Joaquin','El Dorado','Mariposa')`,
  `outFields=NAME,GEOID,BASENAME`, `returnGeometry=true`, `outSR=4326`,
  `f=geojson`.
- **Fetched**: 2026-07-05 (one-time dev fetch; never fetched at runtime).
- **Post-processing**: features sorted by `NAME`; properties reduced to
  `NAME`, `GEOID`, `BASENAME`; coordinates trimmed to 5 decimals (~1.1 m),
  matching the repo-wide GeoJSON precision convention.
- **License**: public domain (U.S. Census Bureau, a U.S. government work).

The file is embedded into the server binary via `embed.go` in this directory
and consumed by `internal/places.Seed`, which upserts one `county:{slug}`
place per feature at boot. Re-fetching with the command above and re-running
the post-processing produces a stable diff-free file for the same vintage.
