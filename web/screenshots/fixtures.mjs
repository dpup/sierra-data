// fixtures.mjs — deterministic mock data for the screenshot harness.
//
// The site fetches every value from same-origin /api/v1/* through api.js. For
// screenshots we intercept those requests (capture.mjs) and answer them from
// here, so pages render populated, realistic state with no server, no API keys,
// and no live upstreams. The scenario is a busy-but-plausible Ebbetts Pass day:
// an active wildfire with an evacuation warning, a Red Flag fire-weather state,
// chain controls and a CHP incident on Hwy 4, weather alerts, a quake, and a
// small MeshCore relay network — enough to exercise every layout.
//
// Shapes mirror what the page render code consumes (protojson camelCase). See
// public/assets/pages/*.js. `routeFor(pathname, searchParams)` returns a plain
// JSON-able object for a known /api/v1 path, or null to let the request 404.

// ---- clock ---------------------------------------------------------------
// Relative times ("4m ago") are computed by the page from Date.now(), so we
// anchor fixtures to the real now to keep those labels sensible.
const now = Date.now();
const ago = (mins) => new Date(now - mins * 60_000).toISOString().replace('.000Z', 'Z');
const ahead = (mins) => new Date(now + mins * 60_000).toISOString().replace('.000Z', 'Z');

/**
 * `ingestedAt` for a revision observed at `iso`.
 *
 * EventRevision.ingested_at is populated on every revision the store writes —
 * it is OUR monotonic stamp for "when this service learned it", as against the
 * upstream-stamped `observed_at`. Omitting it made the History page's ingest-age
 * line unreachable in every screenshot, which is the fixture failure mode the
 * review called out: the UI was verified against a shape the server never emits.
 *
 * Modelled as a short ingest lag, clamped to now — so a FUTURE-dated upstream
 * observation (the mesh clock-skew case) still ingests in the present, which is
 * exactly the discrepancy the History page warns about.
 */
const ingested = (iso) =>
  new Date(Math.min(now, Date.parse(iso) + 90_000)).toISOString().replace('.000Z', 'Z');

// Encode a GeoJSON geometry the way the API carries Place/Event geometry: a
// proto `bytes` field, i.e. base64 of the JSON (format.js decodeGeometry).
const geo = (obj) => Buffer.from(JSON.stringify(obj)).toString('base64');

// ---- places --------------------------------------------------------------

const ebbettsPolygon = {
  type: 'Polygon',
  coordinates: [[
    [-120.18, 38.20], [-119.98, 38.20], [-119.98, 38.40],
    [-120.18, 38.40], [-120.18, 38.20],
  ]],
};
const hwy4Line = {
  type: 'LineString',
  coordinates: [[-120.46, 38.13], [-120.34, 38.19], [-120.19, 38.25], [-120.00, 38.31]],
};

const PLACES = [
  {
    id: 'area:ebbetts-pass', slug: 'ebbetts-pass', name: 'Ebbetts Pass', kind: 'AREA',
    geometry: {
      centroid: { lat: 38.30, lng: -120.08 },
      bbox: { minLat: 38.20, minLng: -120.18, maxLat: 38.40, maxLng: -119.98 },
      geojson: geo(ebbettsPolygon),
    },
  },
  { id: 'county:calaveras-county', slug: 'calaveras-county', name: 'Calaveras County', kind: 'COUNTY',
    geometry: { centroid: { lat: 38.20, lng: -120.55 } } },
  { id: 'county:tuolumne-county', slug: 'tuolumne-county', name: 'Tuolumne County', kind: 'COUNTY',
    geometry: { centroid: { lat: 38.03, lng: -120.24 } } },
  { id: 'town:murphys', slug: 'murphys', name: 'Murphys', kind: 'TOWN', parentId: 'county:calaveras-county',
    geometry: { centroid: { lat: 38.138, lng: -120.461 } } },
  { id: 'town:arnold', slug: 'arnold', name: 'Arnold', kind: 'TOWN', parentId: 'county:calaveras-county',
    geometry: { centroid: { lat: 38.255, lng: -120.352 } } },
  { id: 'town:bear-valley', slug: 'bear-valley', name: 'Bear Valley', kind: 'TOWN', parentId: 'county:alpine-county',
    geometry: { centroid: { lat: 38.481, lng: -120.043 } } },
  {
    id: 'corridor:hwy4-murphys-arnold', slug: 'hwy4-murphys-arnold',
    name: 'Hwy 4 · Murphys → Arnold', kind: 'CORRIDOR', parentId: 'area:ebbetts-pass',
    geometry: {
      centroid: { lat: 38.20, lng: -120.24 },
      bbox: { minLat: 38.13, minLng: -120.46, maxLat: 38.31, maxLng: -120.00 },
      geojson: geo(hwy4Line),
    },
  },
  { id: 'evac_zone:CAL-E043', slug: 'cal-e043', name: 'CAL-E043 (Avery / Hathaway Pines)', kind: 'EVAC_ZONE',
    parentId: 'county:calaveras-county', geometry: { centroid: { lat: 38.20, lng: -120.37 } } },
  // A deliberately QUIET area. The default fixtures exercise the loud paths
  // (null evacuations, an UNAVAILABLE layer, a STALE source); nothing exercised
  // the CALM one, which is the harder assertion to get right — calm is a
  // positive claim requiring every input to be known, so a regression that
  // makes it too easy to reach would never show up in a screenshot without this.
  { id: 'area:quiet-meadow', slug: 'quiet-meadow', name: 'Quiet Meadow', kind: 'AREA',
    geometry: { centroid: { lat: 38.44, lng: -120.22 },
      bbox: { minLat: 38.40, minLng: -120.30, maxLat: 38.48, maxLng: -120.14 } } },
];

/** The slug whose fixtures represent a fully-known, nothing-happening region. */
const CALM_PLACE = 'quiet-meadow';

// ---- events --------------------------------------------------------------
// Each event carries the fields events.js / event-detail.js / mesh.js read.

const EVENTS = [
  {
    id: 'evt-wildfire-mudflat', layer: 'wildfire', severity: 'EXTREME', status: 'ACTIVE',
    wildfire: { acres: 2340, containment: 15, county: 'Calaveras', cause: 'under investigation', hasPerimeter: true },
    headline: 'Mudflat Fire — 2,340 ac, 15% contained', areaLabel: 'Ebbetts Pass', category: 'wildfire',
    observedAt: ago(11), ingestedAt: ingested(ago(11)), revision: 7, provenance: { sourceId: 'calfire', sourceName: 'CAL FIRE', attribution: 'CAL FIRE / FIRIS', sourceUrl: 'https://www.fire.ca.gov/incidents/2026/8/28/mudflat-fire/', fetchedAt: ago(3) },
    description: 'Fast-moving wildfire east of Arnold along the Hwy 4 corridor. Forward spread driven by afternoon winds; spot fires reported north of the highway.',
    summary: 'Extreme fire behavior near Arnold; evacuation warning in effect for zones along Hwy 4.',
    enhancement: { model: 'claude-haiku-4-5', enhancedAt: ago(6), fields: ['summary'] },
    geometry: { centroid: { lat: 38.27, lng: -120.30 },
      geojson: geo({ type: 'Point', coordinates: [-120.30, 38.27] }) },
  },
  {
    id: 'evt-evac-e043', layer: 'evacuation', severity: 'SEVERE', status: 'ACTIVE',
    // eventType is deliberately EMPTY here. The gateway marshals with
    // EmitUnpopulated, so an unfilled string arrives as `""` rather than being
    // omitted — which is how a detail row came to render as a blank line
    // against live data. Kept empty so that path stays covered.
    evacuation: { zoneId: 'CAL-E043', level: 'WARNING', eventType: '', county: 'Calaveras' },
    headline: 'Evacuation WARNING — Zone CAL-E043 (Avery / Hathaway Pines)', areaLabel: 'Calaveras County',
    category: 'evacuation', observedAt: ago(24), ingestedAt: ingested(ago(24)), revision: 2, provenance: { sourceId: 'caloes', sourceName: 'Cal OES', attribution: 'Cal OES — reference only', sourceUrl: 'https://protect.genasys.com/search', fetchedAt: ago(210) },
    description: 'Cal OES / Genasys evacuation warning issued for zone CAL-E043 due to the Mudflat Fire. Be ready to leave.',
    summary: 'Warning (not order) — prepare to evacuate.',
    enhancement: { model: 'claude-haiku-4-5', enhancedAt: ago(6), fields: ['summary'] },
    geometry: { centroid: { lat: 38.20, lng: -120.37 },
      geojson: geo({ type: 'Point', coordinates: [-120.37, 38.20] }) },
  },
  {
    id: 'evt-redflag', layer: 'fire_weather', severity: 'SEVERE', status: 'ACTIVE',
    weatherAlert: { nwsSeverity: 'Severe', certainty: 'Likely', urgency: 'Expected', areaDesc: 'Calaveras and Tuolumne Counties below 5000 ft', zones: ['CAZ138'], instruction: 'Outdoor burning is not recommended. Report new fires immediately.' },
    headline: 'Red Flag Warning — gusts to 40 mph, RH 8%', areaLabel: 'CAZ138 (3000–5000 ft)',
    category: 'fire_weather', observedAt: ago(95), ingestedAt: ingested(ago(95)), revision: 1, provenance: { sourceId: 'nws', sourceName: 'NWS Sacramento CA', attribution: 'NOAA / National Weather Service', sourceUrl: '', fetchedAt: ago(3) },
    description: 'National Weather Service Red Flag Warning in effect through this evening for gusty winds and critically low humidity.',
    summary: 'Critical fire weather; new ignitions may spread rapidly.',
    enhancement: { model: 'claude-haiku-4-5', enhancedAt: ago(6), fields: ['summary'] },
    geometry: { centroid: { lat: 38.25, lng: -120.20 },
      geojson: geo({ type: 'Point', coordinates: [-120.20, 38.25] }) },
  },
  {
    id: 'evt-chp-hwy4', layer: 'road_incident', severity: 'MODERATE', status: 'ACTIVE',
    roadIncident: { logNumber: '250814-0631', impact: 'moderate', duration: '< 1 hour', metadata: { lanesAffected: '1', emergencyServices: 'CHP, Cal Fire' } },
    headline: 'Traffic collision — Hwy 4 near Avery, right lane blocked', areaLabel: 'Hwy 4 · Murphys → Arnold',
    category: 'road_incident', observedAt: ago(6), ingestedAt: ingested(ago(6)), revision: 3, provenance: { sourceId: 'chp', sourceName: 'CHP / Caltrans', attribution: 'quickmap.dot.ca.gov', sourceUrl: '', fetchedAt: ago(1) },
    description: 'Two-vehicle collision blocking the eastbound right lane. Emergency crews on scene; expect delays.',
    summary: 'Right lane blocked near Avery; one-lane traffic control.',
    enhancement: { model: 'claude-haiku-4-5', enhancedAt: ago(6), fields: ['summary'] },
    geometry: { centroid: { lat: 38.19, lng: -120.36 },
      geojson: geo({ type: 'Point', coordinates: [-120.36, 38.19] }) },
  },
  {
    id: 'evt-wx-winter', layer: 'weather_alert', severity: 'MODERATE', status: 'ACTIVE',
    weatherAlert: { nwsSeverity: 'Moderate', certainty: 'Likely', urgency: 'Expected', areaDesc: 'Ebbetts Pass above 5000 ft', zones: ['CAZ139'], instruction: 'Carry chains. Travel over the pass may be difficult.' },
    headline: 'Winter Weather Advisory — 3–6" snow above 5000 ft', areaLabel: 'CAZ139 (above 5000 ft)',
    // sourceName is the ISSUING OFFICE, not the registry row's name, and the
    // empty sourceUrl is deliberate — NWS publishes no per-alert HTML page, only
    // JSON. (Attribution was genuinely empty here until 2026-09-02; the
    // normalizer now sets it. See ingest/weather_alert.go.)
    category: 'weather_alert', observedAt: ago(140), ingestedAt: ingested(ago(140)), revision: 1, provenance: { sourceId: 'nws', sourceName: 'NWS Sacramento CA', attribution: 'NOAA / National Weather Service', sourceUrl: '', fetchedAt: ago(3) },
    description: 'Snow accumulations of 3 to 6 inches above 5000 feet. Chain controls likely on Hwy 4 over Ebbetts Pass.',
    summary: 'Travel over the pass may be difficult; carry chains.',
    enhancement: { model: 'claude-haiku-4-5', enhancedAt: ago(6), fields: ['summary'] },
    geometry: { centroid: { lat: 38.48, lng: -120.04 },
      geojson: geo({ type: 'Point', coordinates: [-120.04, 38.48] }) },
  },
  {
    id: 'evt-quake', layer: 'earthquake', severity: 'MINOR', status: 'ACTIVE',
    earthquake: { magnitude: 3.1, depthKm: 6.2, felt: 14 },
    headline: 'M 3.1 earthquake — 8 km NE of Markleeville', areaLabel: 'Alpine County',
    category: 'earthquake', observedAt: ago(52), ingestedAt: ingested(ago(52)), revision: 1, provenance: { sourceId: 'usgs', sourceName: 'USGS', attribution: 'U.S. Geological Survey', sourceUrl: 'https://earthquake.usgs.gov/earthquakes/map/', fetchedAt: ago(2) },
    description: 'Light earthquake, depth 6.2 km. No damage expected.',
    summary: 'Widely but weakly felt; no damage reports.',
    enhancement: { model: 'claude-haiku-4-5', enhancedAt: ago(6), fields: ['summary'] },
    geometry: { centroid: { lat: 38.75, lng: -119.70 },
      geojson: geo({ type: 'Point', coordinates: [-119.70, 38.75] }) },
  },
  {
    id: 'evt-scheduled-closure', layer: 'road_incident', severity: 'MINOR', status: 'SCHEDULED',
    roadIncident: { logNumber: 'CT-4-PAVE-08', impact: 'moderate', duration: 'ongoing', metadata: { lanesAffected: 'all', permitted: 'true' } },
    headline: 'Planned overnight closure — Hwy 4 paving, Mon 22:00–05:00', areaLabel: 'Hwy 4 · Murphys → Arnold',
    category: 'road_incident', observedAt: ahead(600), ingestedAt: ingested(ahead(600)), revision: 1, provenance: { sourceId: 'chp', sourceName: 'CHP / Caltrans', attribution: 'quickmap.dot.ca.gov', sourceUrl: '', fetchedAt: ago(1) },
    description: 'Scheduled full closure for repaving between Murphys and Avery. Detour via Sheep Ranch Rd.',
    summary: 'Overnight full closure for paving; plan an alternate route.',
    enhancement: { model: 'claude-haiku-4-5', enhancedAt: ago(6), fields: ['summary'] },
    geometry: { centroid: { lat: 38.15, lng: -120.42 },
      geojson: geo({ type: 'Point', coordinates: [-120.42, 38.15] }) },
  },
];

// A small MeshCore relay network (layer=mesh events + /mesh/links topology).
const MESH_NODES = [
  { pk: 'a1b2c3d4e5f6a7b8', name: 'Murphys Repeater', role: 'repeater', lat: 38.138, lng: -120.461, snr: 9, hop: 0, gw: 1 },
  { pk: 'b2c3d4e5f6a7b8c9', name: 'Arnold Hilltop', role: 'repeater', lat: 38.255, lng: -120.352, snr: 6, hop: 1, gw: 0 },
  { pk: 'c3d4e5f6a7b8c9d0', name: 'Bear Valley Room', role: 'room_server', lat: 38.481, lng: -120.043, snr: 4, hop: 2, gw: 0 },
  { pk: 'd4e5f6a7b8c9d0e1', name: 'Avery Companion', role: 'companion', lat: 38.196, lng: -120.366, snr: 3, hop: 2, gw: 0 },
  { pk: 'e5f6a7b8c9d0e1f2', name: 'Dorrington Sensor', role: 'sensor', lat: 38.306, lng: -120.281, snr: 2, hop: 3, gw: 0 },
];

const MESH_EVENTS = MESH_NODES.map((n, i) => ({
  id: `mesh-${n.pk.slice(0, 8)}`, layer: 'mesh', severity: 'INFO', status: 'ACTIVE',
  headline: n.name, areaLabel: 'Ebbetts Pass', category: 'mesh', observedAt: ago(5 + i * 7),
  ingestedAt: ingested(ago(5 + i * 7)),
  revision: 1, provenance: { sourceId: 'meshcore', sourceName: 'MeshCore Mesh', attribution: 'MeshCore community mesh via gomesh.dev', sourceUrl: 'https://map.meshcore.io', fetchedAt: ago(1) },
  mesh: {
    publicKey: n.pk, name: n.name, nodeType: n.role,
    telemetry: { snr: n.snr, hopCount: n.hop, gateways: n.gw ? ['gomesh.dev'] : [] },
  },
  geometry: { centroid: { lat: n.lat, lng: n.lng },
    geojson: geo({ type: 'Point', coordinates: [n.lng, n.lat] }) },
}));

const MESH_LINKS = [
  { a: MESH_NODES[0].pk, b: MESH_NODES[1].pk, observations: 214, daysActive: 22, lastSeen: ago(12), bestSnr: 8 },
  { a: MESH_NODES[1].pk, b: MESH_NODES[2].pk, observations: 88, daysActive: 15, lastSeen: ago(140), bestSnr: 5 },
  { a: MESH_NODES[1].pk, b: MESH_NODES[3].pk, observations: 41, daysActive: 9, lastSeen: ago(38), bestSnr: 4 },
  { a: MESH_NODES[1].pk, b: MESH_NODES[4].pk, observations: 12, daysActive: 4, lastSeen: ago(600), bestSnr: 2 },
];

// ---- sources -------------------------------------------------------------

// The registry as the server actually seeds it (cmd/server gridSourceInfo) —
// ten sources, in the id order ListSources returns. It drifted badly once:
// these were `nifc`, `nws-sto`, `chp-caltrans`, `genasys`, plus `google-routes`
// and `openweather`, which are the roads/weather SERVICES and have never been
// grid sources at all. Every sources screenshot was therefore asserting a
// registry that did not exist. Three pairs here share one poller each
// (calfire/firis, chp/caltrans, pge/psps), which is why their timestamps match
// to the second — that is the real shape of the board, and the reason the names
// carry their feed's subject.
const SOURCES = [
  { id: 'calfire', name: 'CAL FIRE (active incidents)', attribution: 'CAL FIRE', status: 'OK', homepageUrl: 'https://incidents.fire.ca.gov',
    pollIntervalSeconds: 300, lastSuccessAt: ago(2), lastAttemptAt: ago(2), staleAfterSeconds: 900, expireAfterSeconds: 0 },
  // Life-safety, and the reason the UNAVAILABLE row exists at all: a Cal OES
  // failure must render as "unknown — check Genasys", never as zero evacuations.
  { id: 'caloes', name: 'Cal OES (evacuation zones)', attribution: 'Cal OES — reference only', status: 'UNAVAILABLE', homepageUrl: 'https://protect.genasys.com/',
    pollIntervalSeconds: 120, lastSuccessAt: ago(210), lastAttemptAt: ago(1), staleAfterSeconds: 360, expireAfterSeconds: 0,
    lastError: 'HTTP 503 from protect.genasys.com — evacuation status is UNAVAILABLE, not an all-clear' },
  { id: 'caltrans', name: 'Caltrans (chain control + lane closures)', attribution: 'quickmap.dot.ca.gov', status: 'OK', homepageUrl: 'https://quickmap.dot.ca.gov/',
    pollIntervalSeconds: 600, lastSuccessAt: ago(4), lastAttemptAt: ago(4), staleAfterSeconds: 1800, expireAfterSeconds: 0 },
  { id: 'chp', name: 'CHP (traffic incidents)', attribution: 'quickmap.dot.ca.gov', status: 'STALE', homepageUrl: 'https://quickmap.dot.ca.gov/',
    pollIntervalSeconds: 300, lastSuccessAt: ago(23), lastAttemptAt: ago(1), staleAfterSeconds: 900, expireAfterSeconds: 0,
    lastError: 'upstream timeout after 10s (attempt is retrying)' },
  { id: 'firis', name: 'FIRIS (fire perimeters)', attribution: 'CAL FIRE / FIRIS / NIFC', status: 'OK', homepageUrl: 'https://www.caloes.ca.gov/office-of-the-director/operations/response-operations/fire-rescue/firis/',
    pollIntervalSeconds: 300, lastSuccessAt: ago(2), lastAttemptAt: ago(2), staleAfterSeconds: 900, expireAfterSeconds: 86400 },
  { id: 'meshcore', name: 'MeshCore Mesh', attribution: 'MeshCore community mesh', status: 'OK', homepageUrl: 'https://map.meshcore.io',
    pollIntervalSeconds: 60, lastSuccessAt: ago(1), lastAttemptAt: ago(1), staleAfterSeconds: 180, expireAfterSeconds: 7200 },
  { id: 'nws', name: 'National Weather Service (Sacramento)', attribution: 'NOAA / National Weather Service', status: 'OK', homepageUrl: 'https://www.weather.gov/sto',
    pollIntervalSeconds: 300, lastSuccessAt: ago(3), lastAttemptAt: ago(3), staleAfterSeconds: 900, expireAfterSeconds: 86400 },
  { id: 'pge', name: 'PG&E (electric outages)', attribution: 'Pacific Gas and Electric', status: 'OK', homepageUrl: 'https://pgealerts.alerts.pge.com/outage-tools/outage-map/',
    pollIntervalSeconds: 300, lastSuccessAt: ago(3), lastAttemptAt: ago(3), staleAfterSeconds: 900, expireAfterSeconds: 0 },
  // A source that has not completed a poll cycle yet. protojson emits an enum's
  // ZERO value BY NAME, so this arrives as the literal
  // `SOURCE_STATUS_UNSPECIFIED` — observed against a live server, where several
  // feeds sat in this state for the first ~30 seconds after boot. It must render
  // as UNKNOWN: never as OK, and never as the raw proto identifier (which is
  // both a leak and 25 characters wide in a narrow column). Also the only
  // fixture with no lastSuccessAt, so it exercises the "never" cell. Its
  // thresholds ARE populated, because they are config-owned and seeded before
  // the first poll — the "—" threshold rendering is exercised instead by the
  // expireAfterSeconds: 0 ("never auto-expire") rows above, which is where the
  // real API actually produces it.
  { id: 'psps', name: 'PG&E (public safety power shutoffs)', attribution: 'Pacific Gas and Electric', status: 'SOURCE_STATUS_UNSPECIFIED', homepageUrl: 'https://pgealerts.alerts.pge.com/psps-updates/',
    pollIntervalSeconds: 300, lastAttemptAt: ago(1), staleAfterSeconds: 900, expireAfterSeconds: 0 },
  { id: 'usgs', name: 'USGS Earthquakes', attribution: 'U.S. Geological Survey', status: 'OK', homepageUrl: 'https://earthquake.usgs.gov/earthquakes/map/',
    pollIntervalSeconds: 300, lastSuccessAt: ago(2), lastAttemptAt: ago(2), staleAfterSeconds: 900, expireAfterSeconds: 0 },
];

// ---- geojson map layers --------------------------------------------------
// Each layer is an RFC 7946 FeatureCollection with the shared camelCase
// properties envelope + a metadata block carrying sourceStatus honesty.

// lastSourceUpdate is a real field on hazards.Metadata and is what the STALE
// data-age indicator is computed from (generatedAt - lastSourceUpdate). A STALE
// fixture without it silently skips that branch, so STALE defaults to a real age.
const md = (status, extra = {}) => ({
  ...(status === 'STALE' ? { lastSourceUpdate: ago(47) } : {}),
  sourceStatus: status, generatedAt: ago(2), attribution: 'Grid Info Service',
  sourceUrl: 'https://quickmap.dot.ca.gov/', ...extra,
});

const feat = (geometry, properties) => ({ type: 'Feature', geometry, properties });
const pt = (lng, lat) => ({ type: 'Point', coordinates: [lng, lat] });
const line = (coords) => ({ type: 'LineString', coordinates: coords });

const GEOJSON = {
  road_segment: {
    type: 'FeatureCollection', metadata: md('OK', { attribution: 'Google Routes + Caltrans' }),
    features: [
      feat(line([[-120.46, 38.13], [-120.35, 38.19]]), {
        id: 'seg-hwy4-lower', layer: 'road_segment', severity: 'MODERATE', severityRank: 2,
        headline: 'Hwy 4 · Murphys → Avery', status: 'RESTRICTED',
        description: 'Right lane blocked near Avery due to a collision; one-lane traffic control.',
        road: { roadId: 'hwy4-lower', congestion: 'HEAVY', durationMinutes: 18, delayMinutes: 9, distanceKm: 12.4 },
      }),
      feat(line([[-120.35, 38.19], [-120.19, 38.25]]), {
        id: 'seg-hwy4-mid', layer: 'road_segment', severity: 'INFO', severityRank: 0,
        headline: 'Hwy 4 · Avery → Arnold', status: 'OPEN',
        road: { roadId: 'hwy4-mid', congestion: 'LIGHT', durationMinutes: 11, delayMinutes: 0, distanceKm: 9.1 },
      }),
      feat(line([[-120.19, 38.25], [-120.00, 38.31]]), {
        id: 'seg-hwy4-upper', layer: 'road_segment', severity: 'MODERATE', severityRank: 2,
        headline: 'Hwy 4 · Arnold → Bear Valley', status: 'OPEN',
        road: { roadId: 'hwy4-upper', congestion: 'MODERATE', durationMinutes: 26, delayMinutes: 5, distanceKm: 28.7 },
      }),
    ],
  },
  chain_control: {
    type: 'FeatureCollection', metadata: md('OK', { attribution: 'Caltrans' }),
    features: [
      feat(line([[-120.10, 38.34], [-120.00, 38.40]]), {
        id: 'cc-hwy4-r2', layer: 'chain_control', severity: 'MODERATE', severityRank: 2,
        headline: 'Chains or 4WD with snow tires required above Tamarack',
        chainControl: { level: 'R2', highway: 'SR-4', direction: 'Both' },
      }),
      feat(line([[-120.00, 38.40], [-119.98, 38.47]]), {
        id: 'cc-hwy4-r1', layer: 'chain_control', severity: 'MINOR', severityRank: 1,
        headline: 'Chains required except 4WD/AWD with snow tires — Bear Valley',
        chainControl: { level: 'R1', highway: 'SR-4', direction: 'Eastbound' },
      }),
    ],
  },
  road_incident: {
    type: 'FeatureCollection', metadata: md('STALE'),
    features: [
      feat(pt(-120.36, 38.19), {
        id: 'evt-chp-hwy4', layer: 'road_incident', severity: 'MODERATE', severityRank: 2,
    roadIncident: { logNumber: '250814-0631', impact: 'moderate', duration: '< 1 hour', metadata: { lanesAffected: '1', emergencyServices: 'CHP, Cal Fire' } },
        headline: 'Traffic collision — Hwy 4 near Avery, right lane blocked',
        summary: 'Two-vehicle collision blocking the eastbound right lane; crews on scene.',
        description: 'Right lane blocked near Avery; one-lane traffic control.',
        updatedAt: ago(6), incident: { logNumber: '250814-0631' }, source: { name: 'CHP Stockton' },
      }),
    ],
  },
  wildfire: {
    type: 'FeatureCollection', metadata: md('OK', { attribution: 'CAL FIRE FIRIS', sourceUrl: 'https://www.fire.ca.gov/incidents' }),
    features: [
      feat({ type: 'Polygon', coordinates: [[[-120.34, 38.24], [-120.26, 38.24], [-120.26, 38.30], [-120.34, 38.30], [-120.34, 38.24]]] }, {
        id: 'evt-wildfire-mudflat', layer: 'wildfire', severity: 'EXTREME', severityRank: 4,
    wildfire: { acres: 2340, containment: 15, county: 'Calaveras', cause: 'under investigation', hasPerimeter: true },
        headline: 'Mudflat Fire — 2,340 ac, 15% contained', source: { name: 'CAL FIRE' }, updatedAt: ago(11),
      }),
    ],
  },
  fire_weather: {
    type: 'FeatureCollection', metadata: md('OK', { attribution: 'NWS Sacramento' }),
    features: [
      feat({ type: 'Polygon', coordinates: [[[-120.5, 38.1], [-119.9, 38.1], [-119.9, 38.5], [-120.5, 38.5], [-120.5, 38.1]]] }, {
        id: 'fw-redflag', layer: 'fire_weather', severity: 'SEVERE', severityRank: 3,
        headline: 'Red Flag Warning — gusts to 40 mph, RH 8%', fireWeather: { state: 'red-flag' }, source: { name: 'NWS' },
      }),
      // An UNLOCATED feature — `geometry: null` is valid (a zone-wide product
      // that cannot be drawn) and the Map lists it rather than dropping it.
      // It deliberately carries NO `updatedAt`: a live run found this shape,
      // and the Updated column rendered it as a doubled dash. Every other
      // fixture stamps a time, so nothing exercised the absent branch.
      feat(null, {
        id: 'fw-watch-zonewide', layer: 'fire_weather', severity: 'MODERATE', severityRank: 2,
        headline: 'Fire Weather Watch — thunderstorms and strong outflow winds',
        fireWeather: { state: 'elevated' }, source: { name: 'NWS Sacramento CA' },
      }),
    ],
  },
  earthquake: {
    type: 'FeatureCollection', metadata: md('OK', { attribution: 'USGS' }),
    features: [
      feat(pt(-119.70, 38.75), {
        id: 'evt-quake', layer: 'earthquake', severity: 'MINOR', severityRank: 1,
    earthquake: { magnitude: 3.1, depthKm: 6.2, felt: 14 },
        headline: 'M 3.1 — 8 km NE of Markleeville', earthquake: { magnitude: 3.1, depthKm: 6.2 }, source: { name: 'USGS' },
      }),
    ],
  },
  evacuation: {
    type: 'FeatureCollection', metadata: md('UNAVAILABLE', { attribution: 'Genasys Protect', sourceUrl: 'https://protect.genasys.com/' }),
    features: [],
  },
  weather_alert: {
    type: 'FeatureCollection', metadata: md('OK', { attribution: 'NWS Sacramento' }),
    features: [
      feat({ type: 'Polygon', coordinates: [[[-120.2, 38.4], [-119.9, 38.4], [-119.9, 38.6], [-120.2, 38.6], [-120.2, 38.4]]] }, {
        id: 'evt-wx-winter', layer: 'weather_alert', severity: 'MODERATE', severityRank: 2,
    weatherAlert: { nwsSeverity: 'Moderate', certainty: 'Likely', urgency: 'Expected', areaDesc: 'Ebbetts Pass above 5000 ft', zones: ['CAZ139'], instruction: 'Carry chains. Travel over the pass may be difficult.' },
        headline: 'Winter Weather Advisory — 3–6" snow above 5000 ft', source: { name: 'NWS' },
      }),
    ],
  },
  mesh_link: {
    type: 'FeatureCollection', metadata: md('OK', { attribution: 'MeshCore' }),
    features: MESH_NODES.map((n) => feat(pt(n.lng, n.lat), {
      id: `mesh-${n.pk.slice(0, 8)}`, layer: 'mesh_node', severity: 'INFO', severityRank: 0,
      headline: n.name, mesh: { role: n.role, publicKey: n.pk }, source: { name: 'MeshCore' },
    })),
  },
};

// ---- router --------------------------------------------------------------

function eventsFor(params) {
  const layers = params.getAll('layer').map((l) => l.toLowerCase());
  // The calm scenario: nothing above MINOR, so the front page's green state is
  // reachable. Keyed on ?place= so it cannot leak into the default fixtures.
  // page_size is honoured because the front page's "your first request" pane
  // prints the response under a URL that says page_size=1 — returning seven
  // events there would make the pane a lie about its own request.
  const limit = Number(params.get('page_size')) || 0;
  const cap = (list) => (limit > 0 ? list.slice(0, limit) : list);

  if (params.get('place') === CALM_PLACE) {
    const quiet = EVENTS.filter((e) => e.severity === 'MINOR' || e.severity === 'INFO');
    return { events: cap(layers.length ? quiet.filter((e) => layers.includes(e.layer)) : quiet), nextPageToken: '' };
  }
  const pool = [...EVENTS, ...MESH_EVENTS];
  let events = layers.length ? pool.filter((e) => layers.includes(e.layer)) : EVENTS;
  // Default status filter (ACTIVE,SCHEDULED) is the server default; our pool is
  // already active/scheduled, so no extra filtering needed for the demo.
  return { events: cap(events), nextPageToken: '' };
}

function historyFor() {
  const revisions = [];
  for (const e of EVENTS) {
    for (let r = e.revision; r >= 1; r--) {
      const observedAt = new Date(Date.parse(e.observedAt) - (e.revision - r) * 18 * 60_000).toISOString().replace('.000Z', 'Z');
      revisions.push({
        revision: r,
        observedAt,
        ingestedAt: ingested(observedAt),
        event: { id: e.id, headline: e.headline, layer: e.layer, severity: e.severity, status: e.status, observedAt: e.observedAt },
      });
    }
  }
  revisions.sort((a, b) => Date.parse(b.observedAt) - Date.parse(a.observedAt));
  return { revisions, nextPageToken: '' };
}

/**
 * Resolve a /api/v1 request to mock JSON. Returns null for unknown paths.
 * @param {string} pathname e.g. "/api/v1/events"
 * @param {URLSearchParams} params
 * @returns {object|null}
 */
export function routeFor(pathname, params) {
  // Strip the /api prefix the gateway mounts under; accept both /api/v1 and /v1.
  const p = pathname.replace(/^\/api/, '');

  if (p === '/v1/sources') return { sources: SOURCES };
  if (p === '/v1/places') {
    const kind = params.get('kind');
    const places = kind ? PLACES.filter((pl) => pl.kind === kind) : PLACES;
    return { places };
  }
  if (p === '/v1/places:resolve') {
    // Most-specific first: a point in Avery resolves through the nested places.
    return {
      query: { lat: Number(params.get('lat')) || 38.196, lng: Number(params.get('lng')) || -120.366,
        matchedAddress: params.get('address') || undefined },
      places: [
        PLACES.find((x) => x.slug === 'cal-e043'),
        PLACES.find((x) => x.slug === 'murphys'),
        PLACES.find((x) => x.slug === 'hwy4-murphys-arnold'),
        PLACES.find((x) => x.slug === 'calaveras-county'),
        PLACES.find((x) => x.slug === 'ebbetts-pass'),
      ].filter(Boolean),
    };
  }
  if (p === '/v1/events') return eventsFor(params);
  if (p === '/v1/history') return historyFor();
  if (p === '/v1/mesh/links') return { links: MESH_LINKS };

  let m;
  if ((m = p.match(/^\/v1\/events\/([^/]+)\/history$/))) {
    const id = decodeURIComponent(m[1]);
    const e = [...EVENTS, ...MESH_EVENTS].find((x) => x.id === id) || EVENTS[0];
    return {
      revisions: Array.from({ length: e.revision }, (_, i) => e.revision - i).map((r) => {
        const observedAt = new Date(Date.parse(e.observedAt) - (e.revision - r) * 18 * 60_000).toISOString().replace('.000Z', 'Z');
        // Older revisions differ in the fields a source actually revises, not
        // only in the revision number. Every revision used to be `{...e,
        // revision: r}`, so the ONLY diff row ever rendered was `revision 6 → 7`
        // — which meant the diff layout was never tested against the values it
        // exists for: a rewritten headline, a nested detail object, a shifted
        // timestamp. A wildfire's containment climbing is the canonical case.
        const back = e.revision - r; // 0 for the current revision
        const ev = { ...e, revision: r, observedAt };
        if (back > 0) {
          if (e.wildfire) {
            const containment = Math.max(0, (e.wildfire.containment ?? 0) - back * 7);
            ev.wildfire = { ...e.wildfire, containment };
            ev.headline = String(e.headline).replace(
              /(\d+)% contained/,
              `${containment}% contained`
            );
          } else if (e.evacuation) {
            // Warnings escalate to orders; the level and the headline move together.
            ev.evacuation = { ...e.evacuation, level: 'WARNING' };
            ev.headline = String(e.headline).replace('Order', 'Warning');
          } else {
            ev.headline = `${e.headline} (rev ${r})`;
          }
        }
        return { revision: r, observedAt, ingestedAt: ingested(observedAt), event: ev };
      }),
    };
  }
  if ((m = p.match(/^\/v1\/events\/([^/]+)$/))) {
    const id = decodeURIComponent(m[1]);
    return [...EVENTS, ...MESH_EVENTS].find((x) => x.id === id) || EVENTS[0];
  }
  if ((m = p.match(/^\/v1\/places\/([^/]+)\/map\/([^/]+)\.geojson$/))) {
    const layer = decodeURIComponent(m[2]);
    return GEOJSON[layer] || { type: 'FeatureCollection', metadata: md('OK'), features: [] };
  }
  if ((m = p.match(/^\/v1\/places\/([^/]+)\/summary$/))) {
    return placeSummary(decodeURIComponent(m[1]));
  }
  if ((m = p.match(/^\/v1\/places\/([^/]+)$/))) {
    const ref = decodeURIComponent(m[1]);
    return PLACES.find((x) => x.slug === ref || x.id === ref) || PLACES[0];
  }
  return null;
}

function placeSummary(ref) {
  if (ref === CALM_PLACE || ref === 'area:' + CALM_PLACE) {
    return {
      place: CALM_PLACE,
      placeId: 'area:' + CALM_PLACE,
      placeName: 'Quiet Meadow',
      generatedAt: ago(0),
      mode: 'QUIET',
      // `summary` is a SummaryStats MESSAGE, not a string — see PlaceSummary in
      // api/grid/v1/grid.proto. activeEvacuations/evacuationStatus/totalActive/
      // severityCounts all live INSIDE it.
      summary: {
        highestSeverity: 'MINOR',
        highestSeverityRank: 1,
        severityCounts: { MINOR: 1, INFO: 1 },
        totalActive: 2,
        // 0, not null — a CONFIRMED empty, which is what makes calm assertable.
        activeEvacuations: 0,
        evacuationStatus: 'OK',
        topEvents: [],
      },
      domains: [
        { domain: 'fire', status: 'OK', highestSeverity: 'INFO', activeCount: 0, headlines: [] },
        { domain: 'evacuation', status: 'OK', highestSeverity: 'INFO', activeCount: 0, headlines: [] },
        { domain: 'weather', status: 'OK', highestSeverity: 'INFO', activeCount: 0, headlines: [] },
        { domain: 'roads', status: 'OK', highestSeverity: 'MINOR', activeCount: 1, headlines: [
          { id: 'evt-scheduled-closure', severity: 'MINOR', headline: 'Planned overnight closure — Hwy 4 paving' },
        ] },
        { domain: 'seismic', status: 'OK', highestSeverity: 'MINOR', activeCount: 1, headlines: [
          { id: 'evt-quake', severity: 'MINOR', headline: 'M 3.1 earthquake — 8 km NE of Markleeville' },
        ] },
      ],
      sources: SOURCES.filter((x) => x.status === 'OK').slice(0, 5),
    };
  }
  const place = PLACES.find((x) => x.slug === ref || x.id === ref) || PLACES[0];
  return {
    place: place.slug,
    placeId: place.id,
    placeName: place.name,
    generatedAt: ago(0),
    mode: 'ACTIVE',
    // SummaryStats sub-message — mirrors PlaceSummary in grid.proto exactly.
    // Getting this shape wrong makes the UI render "unknown" for the RIGHT
    // reason by accident, which hides real bugs; keep it faithful.
    summary: {
      highestSeverity: 'EXTREME',
      highestSeverityRank: 4,
      severityCounts: { EXTREME: 1, SEVERE: 2, MODERATE: 2, MINOR: 2 },
      totalActive: 6,
      // null = UNAVAILABLE (unknown), never 0. The loud path.
      activeEvacuations: null,
      evacuationStatus: 'UNAVAILABLE',
      topEvents: EVENTS.slice(0, 3).map((e) => ({
        id: e.id, layer: e.layer, severity: e.severity, headline: e.headline,
      })),
    },
    // SummaryDomain.status is the worst SOURCE status across the domain's
    // layers (OK | STALE | UNAVAILABLE) — NOT an activity level. Severity lives
    // in highestSeverity, and headlines is a repeated SummaryDomainHeadline.
    domains: [
      { domain: 'fire', status: 'OK', highestSeverity: 'EXTREME', activeCount: 1, headlines: [
        { id: 'evt-wildfire-mudflat', severity: 'EXTREME', headline: 'Mudflat Fire — 2,340 ac, 15% contained' },
      ] },
      { domain: 'evacuation', status: 'UNAVAILABLE', highestSeverity: 'SEVERE', activeCount: 1, headlines: [
        { id: 'evt-evac-e043', severity: 'SEVERE', headline: 'Evacuation WARNING — Zone CAL-E043 (Avery / Hathaway Pines)' },
      ] },
      { domain: 'weather', status: 'OK', highestSeverity: 'SEVERE', activeCount: 2, headlines: [
        { id: 'evt-redflag', severity: 'SEVERE', headline: 'Red Flag Warning — gusts to 40 mph, RH 8%' },
        { id: 'evt-wx-winter', severity: 'MODERATE', headline: 'Winter Weather Advisory — 3–6" snow above 5000 ft' },
      ] },
      { domain: 'roads', status: 'STALE', highestSeverity: 'MODERATE', activeCount: 2, headlines: [
        { id: 'evt-chp-hwy4', severity: 'MODERATE', headline: 'Traffic collision — Hwy 4 near Avery, right lane blocked' },
      ] },
      { domain: 'seismic', status: 'OK', highestSeverity: 'MINOR', activeCount: 1, headlines: [
        { id: 'evt-quake', severity: 'MINOR', headline: 'M 3.1 earthquake — 8 km NE of Markleeville' },
      ] },
    ],
    // The summary's source-health sidecar. isCalm() checks it for any
    // UNAVAILABLE source, so the loud fixture must actually carry one.
    sources: SOURCES.map((x) => ({ id: x.id, name: x.name, status: x.status, lastSuccessAt: x.lastSuccessAt })),
  };
}

export const FIXTURE_META = { places: PLACES.length, events: EVENTS.length, sources: SOURCES.length };
