// @ts-check
import { defineConfig } from 'astro/config';

// The site is fully static (SSG). Astro renders the shared shell into every page
// at build time; all live data stays as browser `fetch('/v1/…')` in the existing
// public/assets scripts (see web/README.md). Output lands in ../site/dist, which
// the Go server embeds (site/embed.go) — Node runs at build time only.
export default defineConfig({
  site: 'https://data.sierragridteam.org',
  outDir: '../site/dist',
  publicDir: 'public',
  // Emit flat files (sources.html, not sources/index.html) so the URLs and the
  // Go siteHandler's `<name>.html` lookup + clean-URL redirects keep working.
  build: { format: 'file' },
  // No client framework, no view transitions → Astro ships no runtime JS of its
  // own; pages carry only the small inline scripts we author.
  devToolbar: { enabled: false },
});
