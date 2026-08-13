// spec.js — the API's own documentation, as data.
//
// One source of truth for what the endpoints are, what each Event field means,
// and what the severity scale asserts. Both the Docs page (which renders the
// endpoint reference and the envelope table) and the Events detail pane (which
// annotates each live value with its spec sentence) read from here, so a field's
// documentation cannot say two different things on two screens.
//
// The docs-beside-data idea depends on this: a field's spec sentence is shown
// next to a real value from a real record, so the reader never has to trust that
// the prose still matches the API. Keep these strings reconciled with
// api/grid/v1/grid.proto — the proto comments are upstream of this file.
//
// Pure data: no DOM, no network, importable from node for tests.

/** The unified severity scale. rank is the numeric order used by severityRank. */
export const SEV = {
  EXTREME: { rank: 4, note: 'Evacuation Order, an active EXTREME event — act now.' },
  SEVERE: { rank: 3, note: 'Evacuation Warning, a severe wildfire, an M5 quake.' },
  MODERATE: { rank: 2, note: 'Advisory-level. Worth surfacing, not worth waking anyone.' },
  MINOR: { rank: 1, note: 'Minor incident — a lane closure, a small quake.' },
  INFO: { rank: 0, note: 'Ambient state. Mesh-node presence, baseline monitoring.' },
};

/** Severity names, most severe first — the canonical client sort order. */
export const SEV_ORDER = ['EXTREME', 'SEVERE', 'MODERATE', 'MINOR', 'INFO'];

/**
 * What the scale asserts, stated on the Docs page. This is the sentence that
 * stops an integrator mapping our EXTREME onto their "magnitude" field.
 */
export const SEV_CAVEAT =
  'The scale expresses response urgency to the public, not physical magnitude — ' +
  'an Evacuation Order (EXTREME) intentionally outranks an M5 earthquake (SEVERE). ' +
  'Color is never the only signal: every severity is rendered with its label.';

/**
 * Event envelope field documentation: name → [type, spec sentence].
 * Order is the order the envelope table renders in.
 */
export const FIELD_DOCS = {
  id: ['string', 'Globally unique, source-namespaced {namespace}:{native_id} — calfire:, wx:, usgs:, firis:, evac:, chp:, meshcore:.'],
  layer: ['Layer', 'Layer taxonomy. As a value it is the UPPERCASE enum name; as an address it is the lowercase slug.'],
  category: ['string', 'Source sub-type slug, free-form and lowercase ("active", "order").'],
  severity: ['Severity', 'Unified 5-level scale. Response urgency to the public, not physical magnitude.'],
  status: ['EventStatus', 'Lifecycle state: SCHEDULED, ACTIVE, RESOLVED or EXPIRED.'],
  headline: ['string', 'Card-renderable one-liner, kind-agnostic. Treat as untrusted upstream text.'],
  summary: ['string', 'AI-enhanced 2–3 sentences where applicable — see enhancement. Empty when not enhanced.'],
  description: ['string', 'Long form. Original upstream text, always preserved verbatim.'],
  areaLabel: ['string', 'Human area label, e.g. "Hathaway Pines & Avery".'],
  canonicalUrl: ['string', 'Upstream detail URL; scheme-validated, only https:// and http:// survive.'],
  geometry: ['Geometry', '{geojson: bytes, bbox, centroid}. geojson is base64 — decode before parsing. May be absent.'],
  placeIds: ['repeated string', 'Place intersections, precomputed at ingest. Geometry is never touched at query time.'],
  provenance: ['Provenance', 'sourceId, sourceName, attribution, sourceUrl, fetchedAt. Display the attribution wherever you render.'],
  effective: ['Timestamp', 'When the event takes effect — in the future for SCHEDULED.'],
  expires: ['Timestamp', 'Upstream expiry, when the source provides one. Often null.'],
  observedAt: ['Timestamp', 'Upstream last-update time. The trustworthy "as of".'],
  ingestedAt: ['Timestamp', 'When this service ingested the current revision.'],
  revision: ['uint32', 'Monotonic per event. A new revision is written only on content change.'],
  enhancement: ['Enhancement', 'AI-enhancement provenance: model, enhancedAt, fields, plus request/response with ?enhancement_io=true.'],
  wildfire: ['detail', 'acres, containment, county, cause, hasPerimeter.'],
  evacuation: ['detail', 'zoneId, level (ORDER | WARNING | ADVISORY | SHELTER_IN_PLACE), eventType, county. Never AI-rewritten.'],
  weatherAlert: ['detail', 'nwsSeverity, certainty, urgency, instruction, areaDesc, zones[].'],
  earthquake: ['detail', 'magnitude, depthKm, felt.'],
  roadIncident: ['detail', 'logNumber, impact, duration, metadata map.'],
  power: ['detail', 'Outage: outageId, cause, customersAffected, crewStatus, estimatedRestoration. PSPS: eventId, eventName, timePeriod, stage (Watch | Warning), medicalBaselineAffected, deEnergizationStart, deEnergizationEnd. estimatedRestoration and deEnergizationEnd are ESTIMATES PG&E routinely overruns — they are deliberately not mapped onto expires, so never use them to hide an event.'],
  mesh: ['detail', 'publicKey, nodeType, name, telemetry {snr, rssi, hopCount, gateways, lastAdvertAt} — volatile, never mints a revision. Relay paths are NOT here (proto tag reserved): a path belongs to one reception, not to a node, so topology is served derived at GET /api/v1/mesh/links.'],
};

/**
 * The endpoint reference. `examples` are real, runnable URLs — the Docs page
 * turns each into a RUN button that fetches it live and prints the response, so
 * a stale example fails visibly rather than quietly lying.
 */
export const ENDPOINTS = [
  {
    path: '/api/v1/events',
    blurb: 'The cross-layer query — every discrete occurrence through one surface.',
    detail:
      'Returns EventList: {"events": [Event…], "nextPageToken": "…"}. Keyset-paginated in canonical order — severity descending, then observedAt descending, then id. Pass the opaque nextPageToken as page_token for the next page.',
    params: [
      ['place', 'place slug or id', 'Only events intersecting this place, precomputed at ingest.', 'all places'],
      ['layer', 'Layer enum, repeatable', 'Accepts WILDFIRE or the slug wildfire, case-insensitive.', 'all layers'],
      ['status', 'EventStatus, repeatable', 'Include RESOLVED/EXPIRED explicitly to see closed events.', 'ACTIVE,SCHEDULED'],
      ['severity_min', 'Severity enum', 'Inclusive floor on the unified scale.', 'INFO'],
      ['since', 'RFC 3339', 'Only events observed at or after this time.', 'no floor'],
      ['page_size', 'int', 'Max events per page.', 'server default'],
      ['page_token', 'opaque string', "Cursor from a previous response's nextPageToken.", 'first page'],
    ],
    examples: [
      '/api/v1/events?place=ebbetts-pass',
      '/api/v1/events?layer=wildfire&severity_min=MODERATE',
      '/api/v1/events?status=RESOLVED&status=EXPIRED&page_size=10',
    ],
  },
  {
    path: '/api/v1/events/{id}',
    blurb: 'One Event, current revision, as protojson.',
    detail:
      'Ids are source-namespaced — take one from an /events response. Carries a weak ETag; a matching If-None-Match returns 304 and skips the work.',
    params: [],
    examples: ['/api/v1/events?page_size=1'],
  },
  {
    path: '/api/v1/events/{id}/history',
    blurb: 'The per-incident timeline — every revision, newest first.',
    detail:
      'Returns EventRevisionList: each entry is {"revision": n, "observedAt": …, "ingestedAt": …, "event": Event}. A revision is written only when content actually changes; upstream re-stamps with identical content produce nothing. Lifecycle transitions are themselves revisions, so the record ends with the ending.',
    params: [],
    examples: [],
  },
  {
    path: '/api/v1/history',
    blurb: 'Cross-event archive and replay over all revisions — the after-action query.',
    detail: 'Returns EventRevisionList across every event in range.',
    params: [
      ['place', 'place slug or id', 'Revisions of events intersecting this place.', 'all places'],
      ['from', 'RFC 3339', 'Start of the time range (inclusive).', 'open'],
      ['to', 'RFC 3339', 'End of the time range (exclusive).', 'open'],
      ['layer', 'Layer enum, repeatable', 'Filter by layer.', 'all layers'],
    ],
    examples: [
      '/api/v1/history?place=ebbetts-pass&from=2026-08-01T00:00:00Z',
      '/api/v1/history?layer=weather_alert&from=2026-08-01T00:00:00Z',
    ],
  },
  {
    path: '/api/v1/places/{place}/summary',
    blurb: "One fetch returns a place's current state.",
    detail:
      'The area mode (QUIET | WATCH | ACTIVE), a cross-layer summary, per-domain statuses, top events and a source-health sidecar. Carries the evacuation fail-loud invariant: activeEvacuations is int | null, and null means unknown, never zero. An UNAVAILABLE evacuation source forces mode to at least WATCH — unknown is not quiet.',
    params: [],
    examples: ['/api/v1/places/ebbetts-pass/summary'],
  },
  {
    path: '/api/v1/places/{place}/map/{layer}.geojson',
    blurb: 'One RFC 7946 FeatureCollection per layer, ready for MapLibre or Leaflet.',
    detail:
      'Eleven layer slugs: wildfire, evacuation, weather_alert, earthquake, road_incident, power, road_segment, chain_control, fire_weather, mesh_node, mesh_link. A foreign top-level metadata member carries sourceStatus (OK | STALE | UNAVAILABLE), generatedAt, lastSourceUpdate, attribution and sourceUrl. UNAVAILABLE arrives with empty features and must render as an unknown-state banner, never an empty map. Coordinates are [lng, lat], trimmed to 5 decimals. Not in the OpenAPI spec — these layers are hand-built. Cache-Control: max-age=60.',
    params: [],
    examples: [
      '/api/v1/places/ebbetts-pass/map/wildfire.geojson',
      '/api/v1/places/ebbetts-pass/map/evacuation.geojson',
    ],
  },
  {
    path: '/api/v1/places · /api/v1/places/{place}',
    blurb: 'The place directory — areas, counties, towns, corridors.',
    detail:
      'Place ids are {kind}:{slug} and slugs are globally unique, so ebbetts-pass and area:ebbetts-pass both work anywhere a {place} appears. Kinds: AREA, COUNTY, TOWN, EVAC_ZONE, CORRIDOR, SITE. Not paginated — a bounded, slowly-changing directory returned whole.',
    params: [
      ['kind', 'PlaceKind name', 'e.g. COUNTY, TOWN, CORRIDOR.', 'all kinds'],
      ['q', 'string', 'Text filter on place names.', 'no filter'],
    ],
    examples: ['/api/v1/places?kind=COUNTY', '/api/v1/places/town:arnold', '/api/v1/places?q=arnold'],
  },
  {
    path: '/api/v1/places:resolve',
    blurb: 'Point or address → containing places, most-specific-first.',
    detail:
      'Pass lat + lng (WGS84 decimal degrees, pure point-in-polygon, no external calls) or address (one-line, geocoded via the US Census geocoder). Results are ordered SITE, EVAC_ZONE, TOWN, CORRIDOR, COUNTY, AREA.',
    params: [
      ['lat', 'float', 'WGS84 decimal degrees.', '—'],
      ['lng', 'float', 'WGS84 decimal degrees.', '—'],
      ['address', 'string', 'One-line address, geocoded then point-in-polygon.', '—'],
    ],
    examples: ['/api/v1/places:resolve?lat=38.265006&lng=-120.333654'],
  },
  {
    path: '/api/v1/conditions',
    blurb: "Conditions, not events — current weather plus the region's fireWeather classification.",
    detail:
      'fireWeather is normal, elevated or red-flag, derived only from authoritative NWS products. Per-location alerts are dropped: weather alerts are events. There is no roads passthrough either — road conditions are the road_segment and chain_control map layers, road incidents are events.',
    params: [['place', 'place slug or id', "Filters locations to a place's bounding box.", 'all locations']],
    examples: ['/api/v1/conditions', '/api/v1/conditions?place=ebbetts-pass'],
  },
  {
    path: '/api/v1/sources',
    blurb: 'The provenance and health registry behind every sourceStatus in the system.',
    detail:
      'Per source: poll interval, last successful fetch, last attempt, last error text, and derived status OK | STALE | UNAVAILABLE.',
    params: [],
    examples: ['/api/v1/sources'],
  },
  {
    path: '/api/v1/mesh/links',
    blurb: 'MeshCore relay topology — the derived, weighted edge list between nodes.',
    detail:
      'Hand-built (not in the OpenAPI spec). Each advert we hear records its relay path in an append-only observation store; this endpoint serves the rolled-up per-link history. `window` trades currency for coverage. A link is reported with its observation count and last-heard time — a quiet-but-recent link is dim, not absent. Node presence itself is an event (layer=mesh); this is only the edges between them.',
    params: [['window', 'duration slug', 'e.g. 72h, 7d, 30d, all.', '72h']],
    examples: ['/api/v1/mesh/links', '/api/v1/mesh/links?window=7d'],
  },
  {
    path: '/api/v1/scanners',
    blurb: 'Broadcastify scanner-feed configuration — link-out only, no audio rebroadcast.',
    detail: 'Returns ScannerList. Not paginated.',
    params: [['place', 'place slug or id', 'Filter to a place.', 'all']],
    examples: ['/api/v1/scanners?place=ebbetts-pass'],
  },
];

/** Cross-cutting conventions, rendered as definition rows on the front page. */
export const CONVENTIONS = [
  [
    'camelCase / snake_case',
    'Responses are camelCase throughout — observedAt, nextPageToken, sourceStatus. Query parameters stay snake_case regardless: severity_min, page_token, page_size. camelCase forms are also accepted.',
  ],
  [
    'Timestamps',
    'RFC 3339 strings, e.g. 2026-08-06T14:36:57Z. Nullable — expires and observedAt are frequently null.',
  ],
  [
    'Enums',
    'Rendered as proto names: WILDFIRE, ACTIVE, SEVERE. Query parameters accept them case-insensitively, and layer parameters also accept the lowercase slugs used in .geojson paths.',
  ],
  [
    'Repeatable filters',
    'Parameters marked repeatable may appear more than once: ?layer=wildfire&layer=evacuation.',
  ],
  [
    'Pagination',
    "Cursor-based on the event lists only — events, events/{id}/history, history. The directories (places, sources, scanners) return their complete set in one response by design. Don't build paging logic for them.",
  ],
  [
    'ETags',
    'Most reads carry a weak ETag; a matching If-None-Match returns 304. Not yet instrumented: conditions, sources, places:resolve, scanners, summary. Every proto RPC also sends Cache-Control: public, max-age=30 (.geojson uses 60).',
  ],
  [
    'Errors',
    'gRPC-standard {code, codeName, message, details} alongside the mapped HTTP status. The hand-built .geojson endpoint emits the older shape without codeName. Key on the HTTP status and surface message.',
  ],
  ['Canonical client sort', 'For events: severity descending, then observedAt descending.'],
];

/** The eleven .geojson map layers, in the order the Map screen lists them. */
export const MAP_LAYERS = [
  'wildfire',
  'evacuation',
  'weather_alert',
  'earthquake',
  'road_incident',
  'power',
  'road_segment',
  'chain_control',
  'fire_weather',
  'mesh_node',
  'mesh_link',
];

/** Event layer slugs offered as filters on the Events screen. */
export const EVENT_LAYERS = [
  'wildfire',
  'evacuation',
  'weather_alert',
  'earthquake',
  'road_incident',
  'power',
  'mesh',
];
