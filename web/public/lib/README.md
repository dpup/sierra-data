# Browser libraries

MapLibre GL JS comes from the `maplibre-gl` npm dependency in `web/package.json`.
`pnpm run sync:maplibre` copies its ESM bundle, worker, and CSS here before development
or production builds. Generated files are ignored; update the dependency and
commit `pnpm-lock.yaml` to upgrade. The upstream BSD-3-Clause license applies.

Fonts in `fonts/` retain their bundled licenses.
