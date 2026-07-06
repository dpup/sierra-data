import { readFile, readdir } from 'node:fs/promises';
import { join } from 'node:path';
const BOX = ['marginTop','marginBottom','paddingTop','paddingBottom','rowGap','gap'];
// key = "root [NNN]" (DOM position); value = {label, box}
const posKey = p => p.slice(0, p.indexOf(']') + 1);
const posLabel = p => p.slice(p.indexOf(']') + 1).trim();
let totalBox = 0, totalStruct = 0;
for (const f of (await readdir('out/after')).filter(f=>f.endsWith('.metrics.json')).sort()) {
  let before; try { before = JSON.parse(await readFile(join('out/before',f),'utf8')); } catch { continue; }
  const after = JSON.parse(await readFile(join('out/after',f),'utf8'));
  const bMap = new Map(before.map(r => [posKey(r.path), r]));
  const boxLines = [], structLines = [];
  for (const a of after) {
    const b = bMap.get(posKey(a.path)); if (!b) continue;
    if (posLabel(a.path) !== posLabel(b.path)) { structLines.push(`    ${posKey(a.path)} ${posLabel(b.path)} → ${posLabel(a.path)}`); continue; }
    const d = BOX.filter(p => a.box[p]!==b.box[p]).map(p=>`${p}: ${b.box[p]}→${a.box[p]}`);
    if (d.length) boxLines.push(`    ${a.path.replace(/\s+/g,' ')}\n      ${d.join(' | ')}`);
  }
  if (boxLines.length||structLines.length) {
    console.log(`\n## ${f.replace('.metrics.json','')}`);
    if (structLines.length){ console.log(`  structural (label changed at position):`); console.log(structLines.slice(0,8).join('\n')); if(structLines.length>8)console.log(`    …+${structLines.length-8} more`);}
    if (boxLines.length){ console.log(`  box-spacing:`); console.log(boxLines.join('\n')); }
    totalBox+=boxLines.length; totalStruct+=structLines.length;
  }
}
console.log(`\n=== ${totalBox} box-spacing deltas · ${totalStruct} structural (label) shifts ===`);
