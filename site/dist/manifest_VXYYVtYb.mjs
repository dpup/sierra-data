import '@astrojs/internal-helpers/path';
import '@astrojs/internal-helpers/remote';
import 'piccolore';
import 'html-escaper';
import 'clsx';
import { N as NOOP_MIDDLEWARE_HEADER, g as decodeKey } from './chunks/astro/server_2bXGXSRi.mjs';
import 'es-module-lexer';

const NOOP_MIDDLEWARE_FN = async (_ctx, next) => {
  const response = await next();
  response.headers.set(NOOP_MIDDLEWARE_HEADER, "true");
  return response;
};

const codeToStatusMap = {
  // Implemented from IANA HTTP Status Code Registry
  // https://www.iana.org/assignments/http-status-codes/http-status-codes.xhtml
  BAD_REQUEST: 400,
  UNAUTHORIZED: 401,
  PAYMENT_REQUIRED: 402,
  FORBIDDEN: 403,
  NOT_FOUND: 404,
  METHOD_NOT_ALLOWED: 405,
  NOT_ACCEPTABLE: 406,
  PROXY_AUTHENTICATION_REQUIRED: 407,
  REQUEST_TIMEOUT: 408,
  CONFLICT: 409,
  GONE: 410,
  LENGTH_REQUIRED: 411,
  PRECONDITION_FAILED: 412,
  CONTENT_TOO_LARGE: 413,
  URI_TOO_LONG: 414,
  UNSUPPORTED_MEDIA_TYPE: 415,
  RANGE_NOT_SATISFIABLE: 416,
  EXPECTATION_FAILED: 417,
  MISDIRECTED_REQUEST: 421,
  UNPROCESSABLE_CONTENT: 422,
  LOCKED: 423,
  FAILED_DEPENDENCY: 424,
  TOO_EARLY: 425,
  UPGRADE_REQUIRED: 426,
  PRECONDITION_REQUIRED: 428,
  TOO_MANY_REQUESTS: 429,
  REQUEST_HEADER_FIELDS_TOO_LARGE: 431,
  UNAVAILABLE_FOR_LEGAL_REASONS: 451,
  INTERNAL_SERVER_ERROR: 500,
  NOT_IMPLEMENTED: 501,
  BAD_GATEWAY: 502,
  SERVICE_UNAVAILABLE: 503,
  GATEWAY_TIMEOUT: 504,
  HTTP_VERSION_NOT_SUPPORTED: 505,
  VARIANT_ALSO_NEGOTIATES: 506,
  INSUFFICIENT_STORAGE: 507,
  LOOP_DETECTED: 508,
  NETWORK_AUTHENTICATION_REQUIRED: 511
};
Object.entries(codeToStatusMap).reduce(
  // reverse the key-value pairs
  (acc, [key, value]) => ({ ...acc, [value]: key }),
  {}
);

function sanitizeParams(params) {
  return Object.fromEntries(
    Object.entries(params).map(([key, value]) => {
      if (typeof value === "string") {
        return [key, value.normalize().replace(/#/g, "%23").replace(/\?/g, "%3F")];
      }
      return [key, value];
    })
  );
}
function getParameter(part, params) {
  if (part.spread) {
    return params[part.content.slice(3)] || "";
  }
  if (part.dynamic) {
    if (!params[part.content]) {
      throw new TypeError(`Missing parameter: ${part.content}`);
    }
    return params[part.content];
  }
  return part.content.normalize().replace(/\?/g, "%3F").replace(/#/g, "%23").replace(/%5B/g, "[").replace(/%5D/g, "]");
}
function getSegment(segment, params) {
  const segmentPath = segment.map((part) => getParameter(part, params)).join("");
  return segmentPath ? "/" + segmentPath : "";
}
function getRouteGenerator(segments, addTrailingSlash) {
  return (params) => {
    const sanitizedParams = sanitizeParams(params);
    let trailing = "";
    if (addTrailingSlash === "always" && segments.length) {
      trailing = "/";
    }
    const path = segments.map((segment) => getSegment(segment, sanitizedParams)).join("") + trailing;
    return path || "/";
  };
}

function deserializeRouteData(rawRouteData) {
  return {
    route: rawRouteData.route,
    type: rawRouteData.type,
    pattern: new RegExp(rawRouteData.pattern),
    params: rawRouteData.params,
    component: rawRouteData.component,
    generate: getRouteGenerator(rawRouteData.segments, rawRouteData._meta.trailingSlash),
    pathname: rawRouteData.pathname || void 0,
    segments: rawRouteData.segments,
    prerender: rawRouteData.prerender,
    redirect: rawRouteData.redirect,
    redirectRoute: rawRouteData.redirectRoute ? deserializeRouteData(rawRouteData.redirectRoute) : void 0,
    fallbackRoutes: rawRouteData.fallbackRoutes.map((fallback) => {
      return deserializeRouteData(fallback);
    }),
    isIndex: rawRouteData.isIndex,
    origin: rawRouteData.origin
  };
}

function deserializeManifest(serializedManifest) {
  const routes = [];
  for (const serializedRoute of serializedManifest.routes) {
    routes.push({
      ...serializedRoute,
      routeData: deserializeRouteData(serializedRoute.routeData)
    });
    const route = serializedRoute;
    route.routeData = deserializeRouteData(serializedRoute.routeData);
  }
  const assets = new Set(serializedManifest.assets);
  const componentMetadata = new Map(serializedManifest.componentMetadata);
  const inlinedScripts = new Map(serializedManifest.inlinedScripts);
  const clientDirectives = new Map(serializedManifest.clientDirectives);
  const serverIslandNameMap = new Map(serializedManifest.serverIslandNameMap);
  const key = decodeKey(serializedManifest.key);
  return {
    // in case user middleware exists, this no-op middleware will be reassigned (see plugin-ssr.ts)
    middleware() {
      return { onRequest: NOOP_MIDDLEWARE_FN };
    },
    ...serializedManifest,
    assets,
    componentMetadata,
    inlinedScripts,
    clientDirectives,
    routes,
    serverIslandNameMap,
    key
  };
}

const manifest = deserializeManifest({"hrefRoot":"file:///workspace/web/","cacheDir":"file:///workspace/web/node_modules/.astro/","outDir":"file:///workspace/site/dist/","srcDir":"file:///workspace/web/src/","publicDir":"file:///workspace/web/public/","buildClientDir":"file:///workspace/site/dist/client/","buildServerDir":"file:///workspace/site/dist/server/","adapterName":"","routes":[{"file":"file:///workspace/site/dist/docs.html","links":[],"scripts":[],"styles":[{"type":"inline","content":".toc{font-family:var(--font-mono);font-size:12.5px;columns:2;column-gap:2rem;margin:14px 0 26px;padding-left:1.25rem;color:var(--text-2)}.toc li{margin:3px 0;break-inside:avoid}@media(max-width:640px){.toc{columns:1}}.endpoint-head{display:block;margin:28px 0 8px}.endpoint-head .chip-get{font-size:12.5px}.live-examples{margin:4px 0 18px}.live-examples a{display:inline-block;font-family:var(--font-mono);font-size:11.5px;color:var(--blu);background:var(--blu-bg);border:1px solid var(--blu-border);border-radius:var(--radius-sm);padding:2px 8px;margin:3px 5px 3px 0;word-break:break-all}.live-examples a:hover{text-decoration:none;border-color:var(--grn-line)}.live-examples a:before{content:\"GET \";color:var(--grn)}.contract-callout{border:1px solid var(--st-UNAVAILABLE);border-left-width:4px;border-radius:var(--radius);background:color-mix(in srgb,var(--st-UNAVAILABLE) 7%,var(--bg-card));padding:18px 22px;margin:16px 0 24px}.contract-callout>h2:first-child{margin-top:0}.contract-callout .invariant{font-family:var(--font-mono);font-weight:700;color:var(--text-hi);font-size:16px;letter-spacing:.01em;margin:6px 0 12px}section[id]{scroll-margin-top:68px}.backtop{font-family:var(--font-mono);font-size:11px;color:var(--text-dim)}.data-table td{white-space:normal;vertical-align:top;line-height:1.5}.data-table td:first-child,.data-table th,.data-table td.num{white-space:nowrap}\n"}],"routeData":{"route":"/docs","isIndex":false,"type":"page","pattern":"^\\/docs\\/?$","segments":[[{"content":"docs","dynamic":false,"spread":false}]],"params":[],"component":"src/pages/docs.astro","pathname":"/docs","prerender":true,"fallbackRoutes":[],"distURL":[],"origin":"project","_meta":{"trailingSlash":"ignore"}}},{"file":"file:///workspace/site/dist/event.html","links":[],"scripts":[],"styles":[{"type":"inline","content":".crumb-back{margin-bottom:14px}.meta-chip{font-size:10.5px;letter-spacing:.04em;text-transform:uppercase;border:1px solid var(--border-input);border-radius:var(--radius-sm);padding:2px 7px;line-height:1.4;color:var(--text-2)}.ai-chip{border-color:var(--grn-border-2);color:var(--grn)}#ed-head{margin-bottom:4px}#ed-head h1{margin:6px 0;overflow-wrap:anywhere}#ed-lead{margin-top:4px}.query-line{margin:10px 0 4px}.query-row{display:flex;align-items:baseline;gap:10px;padding:3px 0;white-space:nowrap;overflow-x:auto;font-family:var(--font-mono);font-size:12.5px}.raw-toggle{margin-top:12px;font-family:var(--font-mono);font-size:11px}.raw-toggle summary{cursor:pointer;color:var(--text-dim);text-transform:uppercase;letter-spacing:.09em;list-style:none}.raw-toggle summary::-webkit-details-marker{display:none}.raw-toggle summary:before{content:\"▸ \";color:var(--text-fainter)}.raw-toggle[open] summary{color:var(--grn)}.raw-toggle[open] summary:before{content:\"▾ \";color:var(--grn)}.raw-toggle pre.code{margin-top:8px;max-height:26rem;overflow:auto}.ai-badge{display:inline-flex;flex-wrap:wrap;align-items:baseline;gap:4px 9px;border:1px solid var(--grn-border);border-left:3px solid var(--grn-line);border-radius:var(--radius-sm);background:color-mix(in srgb,var(--grn-bg) 70%,var(--bg-card));padding:7px 12px;font-size:12px;margin:4px 0 2px;color:var(--text-2)}.ai-badge-tag{font-family:var(--font-mono);font-weight:600;letter-spacing:.06em;text-transform:uppercase;font-size:10px;color:var(--grn)}.original-text{white-space:pre-wrap;overflow-wrap:anywhere;font-family:var(--font-mono);font-size:12.5px;line-height:1.6;background:var(--bg-code);border:1px solid var(--border);border-left:3px solid var(--border-input);border-radius:var(--radius-sm);padding:10px 13px;margin:0;color:#cfe6da}.enh-body{display:flex;flex-direction:column;gap:10px;align-items:stretch}.enh-body>*{margin:0}.enh-body .ai-badge{align-self:flex-start}.enh-body h3{margin-top:4px;font-size:12px;color:var(--text-2)}.enh-body pre.code{margin:0}.timeline{display:flex;flex-direction:column;gap:12px}.rev-card{border:1px solid var(--border);border-radius:var(--radius);background:var(--bg-card);padding:11px 14px}.rev-card-head{display:flex;flex-wrap:wrap;align-items:baseline;gap:6px 16px;margin-bottom:8px}.diff-wrap{margin:4px 0}.diff-table td.diff-val{max-width:22rem;overflow-wrap:anywhere}.diff-kind{font-family:var(--font-mono);white-space:nowrap}.k-added{color:var(--st-OK)}.k-removed{color:var(--st-UNAVAILABLE)}.k-changed{color:var(--st-STALE)}.timeline-foot{margin-top:12px}\n"}],"routeData":{"route":"/event","isIndex":false,"type":"page","pattern":"^\\/event\\/?$","segments":[[{"content":"event","dynamic":false,"spread":false}]],"params":[],"component":"src/pages/event.astro","pathname":"/event","prerender":true,"fallbackRoutes":[],"distURL":[],"origin":"project","_meta":{"trailingSlash":"ignore"}}},{"file":"file:///workspace/site/dist/events.html","links":[],"scripts":[],"styles":[{"type":"inline","content":".qbar{background:var(--bg-code)}.qbar-line{display:flex;align-items:center;gap:10px;flex-wrap:wrap}.qbar-chips{display:flex;flex-wrap:wrap;gap:8px;flex:1;min-width:0}.qbar-hint{font-size:12px;color:var(--text-dim)}.qbar-copy{margin-left:auto}.qbar-curl{margin-top:10px;overflow-x:auto;white-space:nowrap}.qchip .x{user-select:none}.fspacer{display:block}.results-grid{display:grid;grid-template-columns:minmax(0,1fr) 400px;gap:16px;align-items:start}.results-col{min-width:0}.results-grid .inspector{position:sticky;top:68px;align-self:start;max-height:calc(100vh - 92px);overflow:auto}.ev-insp-body{min-height:140px}.insp-placeholder{color:var(--text-dim)}.events-table tbody tr{cursor:pointer}.events-table tbody tr.ev-selected{background:var(--bg-selected)}.events-table tbody tr.ev-selected td:first-child{box-shadow:inset 3px 0 0 var(--row-sev, var(--grn-line))}.ev-head{color:var(--text);font-size:13.5px;line-height:1.4}.ev-head a{color:var(--text-hi)}.ev-sub{margin-top:3px;font-size:12px;color:var(--text-faint);white-space:normal}.ev-sub .sep{color:var(--text-dimmer)}.results-foot{display:flex;align-items:center;gap:16px;margin-top:14px;margin-bottom:8px}@media(max-width:900px){.results-grid{grid-template-columns:1fr}.results-grid .inspector{position:static;max-height:none}}\n"}],"routeData":{"route":"/events","isIndex":false,"type":"page","pattern":"^\\/events\\/?$","segments":[[{"content":"events","dynamic":false,"spread":false}]],"params":[],"component":"src/pages/events.astro","pathname":"/events","prerender":true,"fallbackRoutes":[],"distURL":[],"origin":"project","_meta":{"trailingSlash":"ignore"}}},{"file":"file:///workspace/site/dist/history.html","links":[],"scripts":[],"styles":[{"type":"inline","content":".layer-checks{display:flex;flex-wrap:wrap;gap:4px 18px;max-width:34rem}.layer-check{display:inline-flex;align-items:center;gap:6px;text-transform:none;letter-spacing:0;font-size:12.5px;color:var(--text);cursor:pointer;white-space:nowrap}.query-line{display:flex;align-items:baseline;gap:12px;font-family:var(--font-mono);font-size:12px;margin:2px 0 14px;overflow-x:auto;white-space:nowrap}tr.day-sep td{background:var(--bg-header);color:var(--text-fainter);font-family:var(--font-mono);font-size:10px;font-weight:600;text-transform:uppercase;letter-spacing:.11em;padding:7px 20px;white-space:nowrap}.data-table td.rev-num,.data-table td.rev-status{color:var(--text-faint)}.results-foot{display:flex;align-items:center;gap:16px;margin-bottom:24px}\n"}],"routeData":{"route":"/history","isIndex":false,"type":"page","pattern":"^\\/history\\/?$","segments":[[{"content":"history","dynamic":false,"spread":false}]],"params":[],"component":"src/pages/history.astro","pathname":"/history","prerender":true,"fallbackRoutes":[],"distURL":[],"origin":"project","_meta":{"trailingSlash":"ignore"}}},{"file":"file:///workspace/site/dist/map.html","links":[],"scripts":[],"styles":[{"type":"inline","content":".map-layout{display:grid;grid-template-columns:300px 1fr;gap:16px;align-items:start}#map-canvas{width:100%;height:560px;border:1px solid var(--border);border-radius:var(--radius);background:#0f1417;position:relative;overflow:hidden}.layer-checks{display:flex;flex-direction:column;gap:2px;margin-top:8px}.layer-check{display:flex;align-items:center;gap:9px;text-transform:none;letter-spacing:0;font-size:12.5px;color:var(--text);cursor:pointer;white-space:nowrap;padding:6px 8px;border-radius:var(--radius-sm)}.layer-check:hover{background:var(--bg-hover-2)}.layer-meta{border-top:1px solid var(--border-row);padding:12px 0;font-size:12.5px}.layer-meta:first-child{border-top:none;padding-top:4px}.layer-meta.ledger-failed{background:var(--bg-failed);border-radius:var(--radius-sm);padding:12px;margin:4px 0}.layer-meta-head{display:flex;justify-content:space-between;align-items:center;gap:10px;margin-bottom:5px;font-family:var(--font-mono)}.meta-row{font-family:var(--font-mono);font-size:11px;padding:1px 0;overflow-wrap:anywhere;color:var(--text-2)}.meta-key{color:var(--text-fainter)}.meta-note{font-size:11px;margin:3px 0}.meta-stale{color:var(--st-STALE)}.meta-unavailable{color:var(--st-UNAVAILABLE);font-weight:600;font-size:12px;margin:4px 0}.error-block.meta-error{margin:6px 0;padding:8px 11px;font-size:11px}.geojson-url{margin-top:6px}.geojson-url-caption{font-size:11px}.geojson-url-line{display:flex;align-items:center;gap:7px}.geojson-url-line code{font-size:11px;background:var(--bg-code);border:1px solid var(--border-soft);border-radius:2px;padding:2px 6px;overflow-wrap:anywhere;user-select:all;color:#9db8cf}.maplibregl-popup-content{background:var(--bg-card);color:var(--text);border:1px solid var(--border);border-radius:var(--radius);font-family:var(--font-mono);font-size:12.5px;padding:10px 12px;box-shadow:0 4px 14px #00000080}.maplibregl-popup-close-button{color:var(--text-faint);font-size:17px}.maplibregl-popup-anchor-bottom .maplibregl-popup-tip,.maplibregl-popup-anchor-bottom-left .maplibregl-popup-tip,.maplibregl-popup-anchor-bottom-right .maplibregl-popup-tip{border-top-color:var(--bg-card)}.maplibregl-popup-anchor-top .maplibregl-popup-tip,.maplibregl-popup-anchor-top-left .maplibregl-popup-tip,.maplibregl-popup-anchor-top-right .maplibregl-popup-tip{border-bottom-color:var(--bg-card)}.maplibregl-popup-anchor-left .maplibregl-popup-tip{border-right-color:var(--bg-card)}.maplibregl-popup-anchor-right .maplibregl-popup-tip{border-left-color:var(--bg-card)}.map-popup .popup-headline{font-weight:600;color:var(--text-hi);margin-bottom:5px}.map-popup .popup-chips{display:flex;align-items:center;gap:8px;margin-bottom:5px}@media(max-width:900px){.map-layout{grid-template-columns:1fr}#map-canvas{height:60vh}}\n"}],"routeData":{"route":"/map","isIndex":false,"type":"page","pattern":"^\\/map\\/?$","segments":[[{"content":"map","dynamic":false,"spread":false}]],"params":[],"component":"src/pages/map.astro","pathname":"/map","prerender":true,"fallbackRoutes":[],"distURL":[],"origin":"project","_meta":{"trailingSlash":"ignore"}}},{"file":"file:///workspace/site/dist/places.html","links":[],"scripts":[],"styles":[],"routeData":{"route":"/places","isIndex":false,"type":"page","pattern":"^\\/places\\/?$","segments":[[{"content":"places","dynamic":false,"spread":false}]],"params":[],"component":"src/pages/places.astro","pathname":"/places","prerender":true,"fallbackRoutes":[],"distURL":[],"origin":"project","_meta":{"trailingSlash":"ignore"}}},{"file":"file:///workspace/site/dist/sources.html","links":[],"scripts":[],"styles":[{"type":"inline","content":".data-table tr.row-STALE{background:color-mix(in srgb,var(--st-STALE) 8%,transparent)}.data-table tr.row-UNAVAILABLE{background:color-mix(in srgb,var(--st-UNAVAILABLE) 11%,transparent)}.data-table td.err-cell{color:var(--st-UNAVAILABLE)}\n"}],"routeData":{"route":"/sources","isIndex":false,"type":"page","pattern":"^\\/sources\\/?$","segments":[[{"content":"sources","dynamic":false,"spread":false}]],"params":[],"component":"src/pages/sources.astro","pathname":"/sources","prerender":true,"fallbackRoutes":[],"distURL":[],"origin":"project","_meta":{"trailingSlash":"ignore"}}},{"file":"file:///workspace/site/dist/index.html","links":[],"scripts":[],"styles":[],"routeData":{"route":"/","isIndex":true,"type":"page","pattern":"^\\/$","segments":[],"params":[],"component":"src/pages/index.astro","pathname":"/","prerender":true,"fallbackRoutes":[],"distURL":[],"origin":"project","_meta":{"trailingSlash":"ignore"}}}],"site":"https://data.sierragridteam.org","base":"/","trailingSlash":"ignore","compressHTML":true,"componentMetadata":[["/workspace/web/src/pages/docs.astro",{"propagation":"none","containsHead":true}],["/workspace/web/src/pages/event.astro",{"propagation":"none","containsHead":true}],["/workspace/web/src/pages/events.astro",{"propagation":"none","containsHead":true}],["/workspace/web/src/pages/history.astro",{"propagation":"none","containsHead":true}],["/workspace/web/src/pages/index.astro",{"propagation":"none","containsHead":true}],["/workspace/web/src/pages/map.astro",{"propagation":"none","containsHead":true}],["/workspace/web/src/pages/places.astro",{"propagation":"none","containsHead":true}],["/workspace/web/src/pages/sources.astro",{"propagation":"none","containsHead":true}]],"renderers":[],"clientDirectives":[["idle","(()=>{var l=(n,t)=>{let i=async()=>{await(await n())()},e=typeof t.value==\"object\"?t.value:void 0,s={timeout:e==null?void 0:e.timeout};\"requestIdleCallback\"in window?window.requestIdleCallback(i,s):setTimeout(i,s.timeout||200)};(self.Astro||(self.Astro={})).idle=l;window.dispatchEvent(new Event(\"astro:idle\"));})();"],["load","(()=>{var e=async t=>{await(await t())()};(self.Astro||(self.Astro={})).load=e;window.dispatchEvent(new Event(\"astro:load\"));})();"],["media","(()=>{var n=(a,t)=>{let i=async()=>{await(await a())()};if(t.value){let e=matchMedia(t.value);e.matches?i():e.addEventListener(\"change\",i,{once:!0})}};(self.Astro||(self.Astro={})).media=n;window.dispatchEvent(new Event(\"astro:media\"));})();"],["only","(()=>{var e=async t=>{await(await t())()};(self.Astro||(self.Astro={})).only=e;window.dispatchEvent(new Event(\"astro:only\"));})();"],["visible","(()=>{var a=(s,i,o)=>{let r=async()=>{await(await s())()},t=typeof i.value==\"object\"?i.value:void 0,c={rootMargin:t==null?void 0:t.rootMargin},n=new IntersectionObserver(e=>{for(let l of e)if(l.isIntersecting){n.disconnect(),r();break}},c);for(let e of o.children)n.observe(e)};(self.Astro||(self.Astro={})).visible=a;window.dispatchEvent(new Event(\"astro:visible\"));})();"]],"entryModules":{"\u0000@astro-page:src/pages/docs@_@astro":"pages/docs.astro.mjs","\u0000@astro-page:src/pages/event@_@astro":"pages/event.astro.mjs","\u0000@astro-page:src/pages/events@_@astro":"pages/events.astro.mjs","\u0000@astro-page:src/pages/history@_@astro":"pages/history.astro.mjs","\u0000@astro-page:src/pages/index@_@astro":"pages/index.astro.mjs","\u0000@astro-page:src/pages/map@_@astro":"pages/map.astro.mjs","\u0000@astro-page:src/pages/places@_@astro":"pages/places.astro.mjs","\u0000@astro-page:src/pages/sources@_@astro":"pages/sources.astro.mjs","\u0000@astro-renderers":"renderers.mjs","\u0000noop-middleware":"_noop-middleware.mjs","\u0000virtual:astro:actions/noop-entrypoint":"noop-entrypoint.mjs","\u0000@astrojs-manifest":"manifest_VXYYVtYb.mjs","astro:scripts/before-hydration.js":""},"inlinedScripts":[],"assets":["/file:///workspace/site/dist/docs.html","/file:///workspace/site/dist/event.html","/file:///workspace/site/dist/events.html","/file:///workspace/site/dist/history.html","/file:///workspace/site/dist/map.html","/file:///workspace/site/dist/places.html","/file:///workspace/site/dist/sources.html","/file:///workspace/site/dist/index.html"],"buildFormat":"file","checkOrigin":false,"allowedDomains":[],"actionBodySizeLimit":1048576,"serverIslandNameMap":[],"key":"JoMiU0jATUrgS42HYbOf4fnq6Ae1aXSiy5Q3sxhbnRw="});
if (manifest.sessionConfig) manifest.sessionConfig.driverModule = null;

export { manifest };
