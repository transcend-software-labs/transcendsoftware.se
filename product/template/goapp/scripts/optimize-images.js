// Build-time image pipeline for generated sites.
//
// PNG/JPEG source files copied into internal/web/static are kept as editable
// originals. This script creates a compact WebP sibling plus responsive widths;
// the Go `asset` helper automatically prefers the sibling, while
// `assetSrcSet` exposes the variants to templates. No npm dependencies: cwebp
// is baked into the Forge sandbox image.

const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');

const ROOT = path.join(__dirname, '..', 'internal', 'web', 'static');
const WIDTHS = [480, 768, 1200, 1600];

function files(dir) {
  const out = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...files(full));
    else if (/\.(png|jpe?g)$/i.test(entry.name)) out.push(full);
  }
  return out;
}

function dimensions(file) {
  const b = fs.readFileSync(file);
  if (b.length >= 24 && b.toString('ascii', 1, 4) === 'PNG') {
    return { width: b.readUInt32BE(16), height: b.readUInt32BE(20) };
  }
  if (b.length >= 4 && b[0] === 0xff && b[1] === 0xd8) {
    let i = 2;
    while (i + 9 < b.length) {
      if (b[i] !== 0xff) { i++; continue; }
      const marker = b[i + 1];
      if ([0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf].includes(marker)) {
        return { height: b.readUInt16BE(i + 5), width: b.readUInt16BE(i + 7) };
      }
      if (marker === 0xd8 || marker === 0xd9) { i += 2; continue; }
      const len = b.readUInt16BE(i + 2);
      if (len < 2) break;
      i += 2 + len;
    }
  }
  return null;
}

function newer(output, input) {
  try { return fs.statSync(output).mtimeMs >= fs.statSync(input).mtimeMs; }
  catch { return false; }
}

function encode(input, output, width, isMark) {
  if (newer(output, input)) return false;
  const args = ['-quiet', '-metadata', 'none', '-m', '6'];
  if (isMark) args.push('-lossless', '-q', '90');
  else args.push('-q', '80', '-sharp_yuv');
  if (width) args.push('-resize', String(width), '0');
  args.push(input, '-o', output);
  const r = spawnSync('cwebp', args, { encoding: 'utf8' });
  if (r.status !== 0) throw new Error(`cwebp failed for ${path.relative(ROOT, input)}: ${(r.stderr || r.stdout).trim()}`);
  return true;
}

function main() {
  const source = files(ROOT);
  if (!source.length) {
    fs.writeFileSync(path.join(ROOT, 'image-manifest.json'), '{}\n');
    console.log('image optimization: no PNG/JPEG assets');
    return;
  }
  const probe = spawnSync('cwebp', ['-version'], { encoding: 'utf8' });
  if (probe.status !== 0) throw new Error('cwebp is required when PNG/JPEG assets are present');

  let written = 0;
  const manifest = {};
  for (const input of source) {
    const ext = path.extname(input);
    const base = input.slice(0, -ext.length);
    const dim = dimensions(input);
    if (!dim) throw new Error(`could not read image dimensions: ${path.relative(ROOT, input)}`);
    manifest[path.relative(ROOT, input).split(path.sep).join('/')] = dim;
    const isMark = /(^|[-_/])(logo|logotyp|mark|icon|ikon)([-_. /]|$)/i.test(input);
    if (encode(input, base + '.webp', 0, isMark)) written++;
    for (const width of WIDTHS) {
      if (width >= dim.width) continue;
      if (encode(input, `${base}-${width}.webp`, width, isMark)) written++;
    }
  }
  fs.writeFileSync(path.join(ROOT, 'image-manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
  console.log(`image optimization: ${source.length} source image(s), ${written} WebP file(s) written`);
}

try { main(); }
catch (e) { console.error('image optimization:', e.message); process.exit(1); }
