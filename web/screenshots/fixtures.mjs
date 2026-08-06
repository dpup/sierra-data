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
];

// ---- events --------------------------------------------------------------
// Each event carries the fields events.js / event-detail.js / mesh.js read.

const EVENTS = [
  {
    id: 'evt-wildfire-mudflat', layer: 'wildfire', severity: 'EXTREME', status: 'ACTIVE',
    headline: 'Mudflat Fire — 2,340 acres, 15% contained', areaLabel: 'Ebbetts Pass', category: 'wildfire',
    observedAt: ago(11), revision: 7, provenance: { sourceId: 'firis' },
    description: 'Fast-moving wildfire east of Arnold along the Hwy 4 corridor. Forward spread driven by afternoon winds; spot fires reported north of the highway.',
    enhancement: {
      summary: 'Extreme fire behavior near Arnold; evacuation warning in effect for zones along Hwy 4.',
      impact: 'severe', duration: 'ongoing',
    },
    geometry: { centroid: { lat: 38.27, lng: -120.30 },
      geojson: geo({ type: 'Point', coordinates: [-120.30, 38.27] }) },
  },
  {
    id: 'evt-evac-e043', layer: 'evacuation', severity: 'SEVERE', status: 'ACTIVE',
    headline: 'Evacuation WARNING — Zone CAL-E043 (Avery / Hathaway Pines)', areaLabel: 'Calaveras County',
    category: 'evacuation', observedAt: ago(24), revision: 2, provenance: { sourceId: 'genasys' },
    description: 'Cal OES / Genasys evacuation warning issued for zone CAL-E043 due to the Mudflat Fire. Be ready to leave.',
    enhancement: { summary: 'Warning (not order) — prepare to evacuate.', impact: 'severe', duration: 'ongoing' },
    geometry: { centroid: { lat: 38.20, lng: -120.37 },
      geojson: geo({ type: 'Point', coordinates: [-120.37, 38.20] }) },
  },
  {
    id: 'evt-redflag', layer: 'fire_weather', severity: 'SEVERE', status: 'ACTIVE',
    headline: 'Red Flag Warning — gusts to 40 mph, RH 8%', areaLabel: 'CAZ138 (3000–5000 ft)',
    category: 'fire_weather', observedAt: ago(95), revision: 1, provenance: { sourceId: 'nws-sto' },
    description: 'National Weather Service Red Flag Warning in effect through this evening for gusty winds and critically low humidity.',
    enhancement: { summary: 'Critical fire weather; new ignitions may spread rapidly.', impact: 'moderate', duration: 'several hours' },
    geometry: { centroid: { lat: 38.25, lng: -120.20 },
      geojson: geo({ type: 'Point', coordinates: [-120.20, 38.25] }) },
  },
  {
    id: 'evt-chp-hwy4', layer: 'road_incident', severity: 'MODERATE', status: 'ACTIVE',
    headline: 'Traffic collision — Hwy 4 near Avery, right lane blocked', areaLabel: 'Hwy 4 · Murphys → Arnold',
    category: 'road_incident', observedAt: ago(6), revision: 3, provenance: { sourceId: 'chp-caltrans' },
    description: 'Two-vehicle collision blocking the eastbound right lane. Emergency crews on scene; expect delays.',
    enhancement: { summary: 'Right lane blocked near Avery; one-lane traffic control.', impact: 'moderate', duration: '< 1 hour' },
    geometry: { centroid: { lat: 38.19, lng: -120.36 },
      geojson: geo({ type: 'Point', coordinates: [-120.36, 38.19] }) },
  },
  {
    id: 'evt-wx-winter', layer: 'weather_alert', severity: 'MODERATE', status: 'ACTIVE',
    headline: 'Winter Weather Advisory — 3–6" snow above 5000 ft', areaLabel: 'CAZ139 (above 5000 ft)',
    category: 'weather_alert', observedAt: ago(140), revision: 1, provenance: { sourceId: 'nws-sto' },
    description: 'Snow accumulations of 3 to 6 inches above 5000 feet. Chain controls likely on Hwy 4 over Ebbetts Pass.',
    enhancement: { summary: 'Travel over the pass may be difficult; carry chains.', impact: 'moderate', duration: 'several hours' },
    geometry: { centroid: { lat: 38.48, lng: -120.04 },
      geojson: geo({ type: 'Point', coordinates: [-120.04, 38.48] }) },
  },
  {
    id: 'evt-quake', layer: 'earthquake', severity: 'MINOR', status: 'ACTIVE',
    headline: 'M 3.1 earthquake — 8 km NE of Markleeville', areaLabel: 'Alpine County',
    category: 'earthquake', observedAt: ago(52), revision: 1, provenance: { sourceId: 'usgs' },
    description: 'Light earthquake, depth 6.2 km. No damage expected.',
    enhancement: { summary: 'Widely but weakly felt; no damage reports.', impact: 'light', duration: 'unknown' },
    geometry: { centroid: { lat: 38.75, lng: -119.70 },
      geojson: geo({ type: 'Point', coordinates: [-119.70, 38.75] }) },
  },
  {
    id: 'evt-scheduled-closure', layer: 'road_incident', severity: 'MINOR', status: 'SCHEDULED',
    headline: 'Planned overnight closure — Hwy 4 paving, Mon 22:00–05:00', areaLabel: 'Hwy 4 · Murphys → Arnold',
    category: 'road_incident', observedAt: ahead(600), revision: 1, provenance: { sourceId: 'chp-caltrans' },
    description: 'Scheduled full closure for repaving between Murphys and Avery. Detour via Sheep Ranch Rd.',
    enhancement: { summary: 'Overnight full closure for paving; plan an alternate route.', impact: 'moderate', duration: 'ongoing' },
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
  revision: 1, provenance: { sourceId: 'meshcore' },
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

const SOURCES = [
  { id: 'firis', name: 'FIRIS (CAL FIRE fire perimeters)', status: 'OK', homepageUrl: 'https://www.fire.ca.gov/',
    pollIntervalSeconds: 300, lastSuccessAt: ago(4), lastAttemptAt: ago(4), staleAfterSeconds: 900, expireAfterSeconds: 3600 },
  { id: 'nws-sto', name: 'NWS Sacramento (alerts + fire weather)', status: 'OK', homepageUrl: 'https://api.weather.gov/',
    pollIntervalSeconds: 300, lastSuccessAt: ago(3), lastAttemptAt: ago(3), staleAfterSeconds: 900, expireAfterSeconds: 3600 },
  { id: 'usgs', name: 'USGS Earthquakes', status: 'OK', homepageUrl: 'https://earthquake.usgs.gov/',
    pollIntervalSeconds: 300, lastSuccessAt: ago(2), lastAttemptAt: ago(2), staleAfterSeconds: 900, expireAfterSeconds: 3600 },
  { id: 'chp-caltrans', name: 'Caltrans / CHP incidents', status: 'STALE', homepageUrl: 'https://quickmap.dot.ca.gov/',
    pollIntervalSeconds: 300, lastSuccessAt: ago(23), lastAttemptAt: ago(1), staleAfterSeconds: 900, expireAfterSeconds: 3600,
    lastError: 'upstream timeout after 10s (attempt is retrying)' },
  { id: 'google-routes', name: 'Google Routes (travel times)', status: 'OK', homepageUrl: 'https://developers.google.com/maps/documentation/routes',
    pollIntervalSeconds: 2700, lastSuccessAt: ago(18), lastAttemptAt: ago(18), staleAfterSeconds: 5400, expireAfterSeconds: 10800 },
  { id: 'genasys', name: 'Genasys Protect (evacuation zones)', status: 'UNAVAILABLE', homepageUrl: 'https://protect.genasys.com/',
    pollIntervalSeconds: 300, lastSuccessAt: ago(210), lastAttemptAt: ago(1), staleAfterSeconds: 900, expireAfterSeconds: 3600,
    lastError: 'HTTP 503 from protect.genasys.com — evacuation status is UNAVAILABLE, not an all-clear' },
  { id: 'openweather', name: 'OpenWeatherMap (current conditions)', status: 'OK', homepageUrl: 'https://openweathermap.org/',
    pollIntervalSeconds: 900, lastSuccessAt: ago(7), lastAttemptAt: ago(7), staleAfterSeconds: 1800, expireAfterSeconds: 3600 },
  { id: 'meshcore', name: 'MeshCore MQTT bridge (gomesh.dev)', status: 'OK', homepageUrl: 'https://gomesh.dev/',
    pollIntervalSeconds: 60, lastSuccessAt: ago(1), lastAttemptAt: ago(1), staleAfterSeconds: 259200, expireAfterSeconds: 604800 },
];

// ---- geojson map layers --------------------------------------------------
// Each layer is an RFC 7946 FeatureCollection with the shared camelCase
// properties envelope + a metadata block carrying sourceStatus honesty.

const md = (status, extra = {}) => ({
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
        headline: 'Mudflat Fire — 2,340 acres, 15% contained', source: { name: 'CAL FIRE' }, updatedAt: ago(11),
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
    ],
  },
  earthquake: {
    type: 'FeatureCollection', metadata: md('OK', { attribution: 'USGS' }),
    features: [
      feat(pt(-119.70, 38.75), {
        id: 'evt-quake', layer: 'earthquake', severity: 'MINOR', severityRank: 1,
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
        headline: 'Winter Weather Advisory — 3–6" snow above 5000 ft', source: { name: 'NWS' },
      }),
    ],
  },
  mesh_node: {
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
  const pool = [...EVENTS, ...MESH_EVENTS];
  let events = layers.length ? pool.filter((e) => layers.includes(e.layer)) : EVENTS;
  // Default status filter (ACTIVE,SCHEDULED) is the server default; our pool is
  // already active/scheduled, so no extra filtering needed for the demo.
  return { events, nextPageToken: '' };
}

function historyFor() {
  const revisions = [];
  for (const e of EVENTS) {
    for (let r = e.revision; r >= 1; r--) {
      revisions.push({
        revision: r,
        observedAt: new Date(Date.parse(e.observedAt) - (e.revision - r) * 18 * 60_000).toISOString().replace('.000Z', 'Z'),
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
      revisions: Array.from({ length: e.revision }, (_, i) => e.revision - i).map((r) => ({
        revision: r,
        observedAt: new Date(Date.parse(e.observedAt) - (e.revision - r) * 18 * 60_000).toISOString().replace('.000Z', 'Z'),
        event: { ...e, revision: r },
      })),
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
  return {
    place: PLACES.find((x) => x.slug === ref || x.id === ref) || PLACES[0],
    mode: 'ACTIVE',
    summary: 'Active wildfire with an evacuation warning along Hwy 4; Red Flag fire weather and chain controls over the pass.',
    activeEvacuations: null,
    totalActive: 6,
    domains: [
      { domain: 'fire', status: 'ACTIVE', headline: 'Mudflat Fire — 2,340 acres, 15% contained' },
      { domain: 'evacuation', status: 'UNAVAILABLE', headline: 'Genasys unreachable — check protect.genasys.com' },
      { domain: 'weather', status: 'WATCH', headline: 'Red Flag Warning; winter advisory above 5000 ft' },
      { domain: 'roads', status: 'WATCH', headline: 'Collision + chain controls on Hwy 4' },
      { domain: 'seismic', status: 'OK', headline: 'M 3.1 near Markleeville' },
    ],
  };
}

export const FIXTURE_META = { places: PLACES.length, events: EVENTS.length, sources: SOURCES.length };
