import { copyFile, mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
const dist = dirname(fileURLToPath(import.meta.resolve('maplibre-gl')));
const target = new URL('../public/lib/', import.meta.url);
await mkdir(target, { recursive: true });
for (const file of ['maplibre-gl.mjs', 'maplibre-gl-worker.mjs', 'maplibre-gl.css']) {
  await copyFile(resolve(dist, file), new URL(file, target));
}
