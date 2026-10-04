// Precompresses the built bundle: writes <file>.br (brotli, max quality) and
// <file>.gz (gzip -9) next to every compressible file in the build output,
// which the Go server's static handler (servePrecompressed in
// internal/api/handlers.go) serves to clients that accept the encoding.
// Without them every asset was compressed on every request — the main chunk
// alone cost 26 ms of CPU with brotli. Max quality is affordable once, at
// build time; it is not per request.
//
//   node scripts/precompress.mjs [dir]   (default: dist)
//
// Run after `vite build` (`npm run precompress`); the Docker image runs it on
// the map-stripped copy. Source maps are skipped: the server never serves them.
// No dependencies beyond node:zlib.
import {readdirSync, readFileSync, rmSync, statSync, writeFileSync} from 'node:fs'
import {extname, join} from 'node:path'
import process from 'node:process'
import {brotliCompressSync, constants, gzipSync} from 'node:zlib'

// Text formats and wasm. Images, fonts (woff2 is brotli already) and archives
// are compressed formats; html is left out because index.html is served as
// is, with its own cache policy and CSP pin derived from the file.
const COMPRESSIBLE = new Set(['.js', '.mjs', '.css', '.svg', '.json', '.wasm'])

// The server does not compress dynamic responses under 1 KB either
// (minCompressSize in internal/api/middleware/compress.go): below it the
// saving is a few hundred bytes that fit one TCP segment anyway.
const MIN_SIZE = 1024

function* walk(dir) {
  for (const entry of readdirSync(dir, {withFileTypes: true})) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) yield* walk(path)
    else if (entry.isFile()) yield path
  }
}

function encode(data, ext) {
  const brotli = brotliCompressSync(data, {
    params: {
      [constants.BROTLI_PARAM_QUALITY]: constants.BROTLI_MAX_QUALITY,
      [constants.BROTLI_PARAM_MODE]: ext === '.wasm' ? constants.BROTLI_MODE_GENERIC : constants.BROTLI_MODE_TEXT,
      [constants.BROTLI_PARAM_SIZE_HINT]: data.length,
    },
  })
  const gzip = gzipSync(data, {level: constants.Z_BEST_COMPRESSION})
  return [
    ['.br', brotli],
    ['.gz', gzip],
  ]
}

const root = process.argv[2] ?? 'dist'
let files = 0
let before = 0
let afterBrotli = 0
for (const path of walk(root)) {
  const ext = extname(path)
  if (!COMPRESSIBLE.has(ext) || statSync(path).size < MIN_SIZE) continue
  const data = readFileSync(path)
  files++
  before += data.length
  for (const [suffix, encoded] of encode(data, ext)) {
    if (encoded.length < data.length) {
      writeFileSync(path + suffix, encoded)
      if (suffix === '.br') afterBrotli += encoded.length
    } else {
      // Not worth serving; drop one a previous run may have left.
      rmSync(path + suffix, {force: true})
      if (suffix === '.br') afterBrotli += data.length
    }
  }
}
const kb = n => `${(n / 1024).toFixed(0)} KB`
process.stdout.write(`precompress: ${files} files in ${root}, ${kb(before)} -> ${kb(afterBrotli)} brotli\n`)
