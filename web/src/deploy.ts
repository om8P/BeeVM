// Deploying a function from the browser: everything except the upload itself, as plain
// functions so it can be tested without a browser.
//
// The rules mirror the CLI and the server (internal/pkg/pkg.go, cmd/beevm/commands.go,
// internal/registry): checking them here only gives faster, clearer errors; the server still
// checks everything itself. If you change a rule there, change it here too.

export type Runtime = 'binary' | 'python' | 'node'

// Limits (internal/agentproto/agentproto.go).
export const MAX_UPLOAD_BYTES = 50 << 20 // a function's code, zip or binary
export const MAX_PACKAGE_ENTRIES = 2000 // files in a python/node package
export const MAX_UNPACKED_BYTES = 32 << 20 // a package's size once unpacked

// Settings ranges (internal/registry).
export const MEMORY_MIB = { min: 64, max: 1024, def: 128 }
export const VCPUS = { min: 1, max: 4, def: 1 }
export const TIMEOUT_S = { min: 1, max: 60, def: 10 }

// The CLI's folder zipper skips these folders (internal/pkg: skipDirs).
const SKIP_DIRS = new Set(['.git', '__pycache__'])

/** A file chosen in the browser: its path inside the chosen folder (or just its name) and bytes. */
export interface SourceFile {
  path: string
  data: Uint8Array
}

/** What the user picked: one file, or a folder's files (paths relative to that folder). */
export type Source = { kind: 'file'; file: SourceFile } | { kind: 'folder'; name: string; files: SourceFile[] }

/** Fields the user may set; empty / undefined means "use the default". */
export interface Overrides {
  runtime?: Runtime | ''
  handler?: string
  memoryMiB?: number
  vcpus?: number
  timeoutS?: number
}

/** The deploy config part, as the server reads it (apitypes.FunctionConfig). */
export interface DeployConfig {
  runtime: Runtime
  handler?: string
  memory_mib?: number
  vcpus?: number
  timeout_ms?: number
}

/** Everything needed for the upload, plus what to tell the user. */
export interface Prepared {
  runtime: Runtime
  handler: string // the handler that will be used ('' for binaries)
  code: Uint8Array
  filename: string
  config: DeployConfig
  warnings: string[]
  files: number // files in the package (1 for a binary)
}

/** A rule the chosen code or settings break; the message says which. */
export class DeployError extends Error {}

// ---------------------------------------------------------------- names and handlers

const NAME_RE = /^[a-z0-9][a-z0-9-]{0,62}$/

/** nameError says what is wrong with a function name, or '' when it is fine. */
export function nameError(name: string): string {
  if (name === '') return 'Give the function a name.'
  if (!NAME_RE.test(name)) return 'Use lowercase letters, digits and "-", 1 to 63 characters, starting with a letter or digit.'
  return ''
}

/** suggestName turns a file or folder name into a valid function name (like beevm init). */
export function suggestName(fileName: string): string {
  const base = fileName.replace(/\.(py|js|mjs|cjs|zip)$/i, '')
  const n = base.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63).replace(/-+$/, '')
  return n || 'my-function'
}

export function defaultHandler(rt: Runtime): string {
  return rt === 'python' ? 'handler.handler' : rt === 'node' ? 'index.handler' : ''
}

const PY_IDENT = '[A-Za-z_][A-Za-z0-9_]*'
const PY_HANDLER = new RegExp(`^${PY_IDENT}(\\.${PY_IDENT})*\\.${PY_IDENT}$`)
const NODE_HANDLER = /^[A-Za-z0-9_][A-Za-z0-9_./-]*\.[A-Za-z_$][A-Za-z0-9_$]*$/

/** handlerError checks a handler's syntax for the runtime ('' = the default, always fine). */
export function handlerError(rt: Runtime, handler: string): string {
  if (handler === '') return ''
  if (rt === 'python' && !PY_HANDLER.test(handler)) {
    return `Handler "${handler}" must look like module.function (for example handler.handler).`
  }
  if (rt === 'node' && (!NODE_HANDLER.test(handler) || handler.includes('..'))) {
    return `Handler "${handler}" must look like file.function (for example index.handler).`
  }
  if (rt === 'binary') return 'Binaries have no handler: leave it empty.'
  return ''
}

/** handlerFiles lists the files, one of which the package needs for the handler. */
export function handlerFiles(rt: Runtime, handler: string): string[] {
  const h = handler || defaultHandler(rt)
  const module = h.slice(0, Math.max(h.lastIndexOf('.'), 0))
  if (rt === 'python') {
    const p = module.replaceAll('.', '/')
    return [`${p}.py`, `${p}/__init__.py`]
  }
  if (rt === 'node') return [`${module}.js`, `${module}.mjs`, `${module}.cjs`]
  return []
}

// ---------------------------------------------------------------- detection

const ZIP_MAGIC = [0x50, 0x4b, 0x03, 0x04]
const ELF_MAGIC = [0x7f, 0x45, 0x4c, 0x46]

const startsWith = (data: Uint8Array, magic: number[]) => magic.every((b, i) => data[i] === b)
export const isZip = (data: Uint8Array) => startsWith(data, ZIP_MAGIC)
export const isElf = (data: Uint8Array) => startsWith(data, ELF_MAGIC)

/** runtimeFromNames: handler.py at the top -> python, index.js/.mjs/.cjs -> node. */
export function runtimeFromNames(names: Iterable<string>): Runtime {
  const set = new Set(names)
  const py = set.has('handler.py')
  const node = set.has('index.js') || set.has('index.mjs') || set.has('index.cjs')
  if (py && node) throw new DeployError('This has both handler.py and index.js: choose the runtime.')
  if (py) return 'python'
  if (node) return 'node'
  throw new DeployError('No handler.py or index.js at the top: choose the runtime and give the handler.')
}

/** runtimeOfFile guesses a single file's runtime, or says it is a zip package. */
export function runtimeOfFile(name: string, data: Uint8Array): Runtime | 'zip' {
  const ext = name.toLowerCase().match(/\.[^./]+$/)?.[0] ?? ''
  if (ext === '.py') return 'python'
  if (ext === '.js' || ext === '.mjs' || ext === '.cjs') return 'node'
  if (isZip(data)) return 'zip'
  if (isElf(data)) return 'binary'
  throw new DeployError(`Cannot tell what ${name} is: choose a .py or .js file, a .zip, a folder, or a Linux program.`)
}

// ---------------------------------------------------------------- Linux programs

/**
 * elfCheck reads a Linux program's header like the CLI does (cmd/beevm: checkBinary): it must
 * be a 64-bit x86-64 executable. Returns a warning for a dynamically linked one: the guest is
 * Alpine (musl), so a program built against glibc will not start.
 */
export function elfCheck(data: Uint8Array): string {
  if (!isElf(data) || data.length < 64) {
    throw new DeployError('This is not a Linux program. Build a static Linux x86-64 binary, for Go: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build')
  }
  if (data[4] !== 2 || data[5] !== 1) throw new DeployError('This program is not 64-bit little-endian; the guest runs x86-64.')
  const v = new DataView(data.buffer, data.byteOffset, data.byteLength)
  const type = v.getUint16(16, true)
  const machine = v.getUint16(18, true)
  if (machine !== 62) throw new DeployError(`This program is built for another CPU (machine ${machine}); the guest runs x86-64.`)
  if (type !== 2 && type !== 3) throw new DeployError('This file is not an executable program.')
  const phoff = Number(v.getBigUint64(32, true))
  const phentsize = v.getUint16(54, true)
  const phnum = v.getUint16(56, true)
  for (let i = 0; i < phnum; i++) {
    const at = phoff + i * phentsize
    if (at + 4 > data.length) break
    if (v.getUint32(at, true) === 3 /* PT_INTERP */) {
      return 'This program is dynamically linked. The guest is Alpine (musl), so a glibc program will not start. Build it static (for Go: CGO_ENABLED=0).'
    }
  }
  return ''
}

// ---------------------------------------------------------------- packages

/** One entry of a package, as the server's rules see it. */
export interface Entry {
  name: string
  size: number
  dir?: boolean
}

/** entriesError applies the server's package rules (internal/pkg: checkEntries). */
export function entriesError(entries: Entry[]): string {
  if (entries.length === 0) return 'The package is empty.'
  if (entries.length > MAX_PACKAGE_ENTRIES) return `${entries.length} files; at most ${MAX_PACKAGE_ENTRIES} are allowed.`
  let total = 0
  for (const e of entries) {
    const n = e.name
    if (n === '' || n.includes('\\') || n.startsWith('/') || n.includes('\0')) return `Bad file name "${n}".`
    const clean = cleanPath(n)
    if (clean === '.' || clean === '..' || clean.startsWith('../')) return `File name "${n}" points outside the package.`
    if (!e.dir) total += e.size
    if (total > MAX_UNPACKED_BYTES) return `Unpacked size over ${MAX_UNPACKED_BYTES >> 20} MiB.`
  }
  return ''
}

/** cleanPath is Go's path.Clean for relative slash paths (enough for the rules above). */
export function cleanPath(p: string): string {
  const out: string[] = []
  for (const part of p.split('/')) {
    if (part === '' || part === '.') continue
    if (part === '..' && out.length > 0 && out[out.length - 1] !== '..') out.pop()
    else out.push(part)
  }
  return out.join('/') || '.'
}

/** missingHandlerFile says which file the handler needs when the package lacks it, else ''. */
export function missingHandlerFile(rt: Runtime, handler: string, names: Iterable<string>): string {
  const have = new Set([...names].map(cleanPath))
  const want = handlerFiles(rt, handler)
  if (want.some((f) => have.has(f))) return ''
  return `Handler ${handler || defaultHandler(rt)} needs ${want.join(' or ')} at the top of the package (not inside a folder).`
}

/** folderFiles keeps the files the CLI would zip: everything except .git and __pycache__. */
export function folderFiles(files: SourceFile[]): SourceFile[] {
  return files.filter((f) => !f.path.split('/').some((part) => SKIP_DIRS.has(part)))
}

/**
 * stripRoot removes the chosen folder's own name from browser paths: a folder picker reports
 * "my-fn/handler.py", but the package must have handler.py at the top. Only strips when every
 * path starts with the same folder.
 */
export function stripRoot(paths: string[]): string[] {
  if (paths.length === 0) return paths
  const first = paths[0].split('/')[0]
  if (!paths.every((p) => p.includes('/') && p.split('/')[0] === first)) return paths
  return paths.map((p) => p.slice(first.length + 1))
}

/** zipEntries lists a zip's entries without unpacking anything. */
export async function zipEntries(data: Uint8Array): Promise<Entry[]> {
  const { unzipSync } = await import('fflate')
  const entries: Entry[] = []
  try {
    unzipSync(data, {
      filter: (f) => {
        entries.push({ name: f.name, size: f.originalSize, dir: f.name.endsWith('/') })
        return false // only the names and sizes, nothing is decompressed
      },
    })
  } catch (e) {
    throw new DeployError(`Not a valid zip file (${e instanceof Error ? e.message : String(e)}).`)
  }
  return entries
}

/** makeZip builds a package from files (paths relative to its top level). */
export async function makeZip(files: SourceFile[]): Promise<Uint8Array> {
  const { zipSync } = await import('fflate')
  const input: Record<string, Uint8Array> = {}
  for (const f of files) input[f.path] = f.data
  return zipSync(input, { level: 6 })
}

// ---------------------------------------------------------------- putting it together

/**
 * prepare turns what the user picked plus their settings into the upload: it detects the
 * runtime (unless chosen), zips files and folders for python/node, checks every rule the
 * server would check, and builds the config part. Throws DeployError with a message for the
 * user when something is wrong.
 */
export async function prepare(src: Source, o: Overrides = {}): Promise<Prepared> {
  const warnings: string[] = []
  let runtime: Runtime
  let handler = (o.handler ?? '').trim()
  let code: Uint8Array
  let filename: string
  let names: string[]
  let entries: Entry[]

  if (src.kind === 'folder') {
    const files = folderFiles(src.files)
    const paths = stripRoot(files.map((f) => f.path))
    const pkgFiles = files.map((f, i) => ({ path: paths[i], data: f.data }))
    if (pkgFiles.length === 0) throw new DeployError(`${src.name} has no files.`)
    names = pkgFiles.map((f) => f.path)
    runtime = o.runtime || runtimeFromNames(names)
    if (runtime === 'binary') throw new DeployError('A folder cannot be a binary: choose the program file itself.')
    entries = pkgFiles.map((f) => ({ name: f.path, size: f.data.length }))
    const bad = entriesError(entries)
    if (bad) throw new DeployError(bad)
    code = await makeZip(pkgFiles)
    filename = `${suggestName(src.name)}.zip`
  } else {
    const f = src.file
    const kind = runtimeOfFile(f.path, f.data)
    if (kind === 'zip') {
      entries = await zipEntries(f.data)
      names = entries.filter((e) => !e.dir).map((e) => cleanPath(e.name))
      runtime = o.runtime || runtimeFromNames(names)
      if (runtime === 'binary') throw new DeployError('A zip file cannot be a binary: choose the program file itself.')
      const bad = entriesError(entries)
      if (bad) throw new DeployError(bad)
      code = f.data
      filename = f.path
    } else if (kind === 'binary' || o.runtime === 'binary') {
      runtime = 'binary'
      const warning = elfCheck(f.data)
      if (warning) warnings.push(warning)
      code = f.data
      filename = f.path
      names = []
    } else {
      // One source file: zipped under its own name, and like the CLI the handler defaults to
      // "<file name>.handler" (app.py -> app.handler).
      runtime = o.runtime || kind
      if (handler === '') handler = `${f.path.replace(/\.[^.]+$/, '')}.handler`
      names = [f.path]
      code = await makeZip([f])
      filename = `${suggestName(f.path)}.zip`
    }
  }

  if (runtime === 'binary') {
    if (handler !== '') throw new DeployError('Binaries have no handler: leave it empty.')
  } else {
    const he = handlerError(runtime, handler)
    if (he) throw new DeployError(he)
    const missing = missingHandlerFile(runtime, handler, names)
    if (missing) throw new DeployError(missing)
  }
  if (code.length > MAX_UPLOAD_BYTES) {
    throw new DeployError(`The code is ${(code.length / (1 << 20)).toFixed(1)} MiB; the upload limit is ${MAX_UPLOAD_BYTES >> 20} MiB.`)
  }

  return {
    runtime,
    handler: runtime === 'binary' ? '' : handler || defaultHandler(runtime),
    code,
    filename,
    config: deployConfig(runtime, handler, o),
    warnings,
    files: runtime === 'binary' ? 1 : names.length,
  }
}

/** settingError checks the memory, vCPU and timeout fields ('' when fine or empty). */
export function settingError(o: Overrides): string {
  const bad = (v: number | undefined, r: { min: number; max: number }) =>
    v !== undefined && (!Number.isInteger(v) || v < r.min || v > r.max)
  if (bad(o.memoryMiB, MEMORY_MIB)) return `Memory must be ${MEMORY_MIB.min} to ${MEMORY_MIB.max} MiB.`
  if (bad(o.vcpus, VCPUS)) return `vCPUs must be ${VCPUS.min} to ${VCPUS.max}.`
  if (bad(o.timeoutS, TIMEOUT_S)) return `Timeout must be ${TIMEOUT_S.min} to ${TIMEOUT_S.max} seconds.`
  return ''
}

/** deployConfig builds the config part. The runtime is always sent: the server takes a missing
 * runtime as "binary". Other fields only when set, so the server's defaults apply. */
export function deployConfig(runtime: Runtime, handler: string, o: Overrides): DeployConfig {
  const c: DeployConfig = { runtime }
  if (runtime !== 'binary' && handler) c.handler = handler
  if (o.memoryMiB) c.memory_mib = o.memoryMiB
  if (o.vcpus) c.vcpus = o.vcpus
  if (o.timeoutS) c.timeout_ms = o.timeoutS * 1000
  return c
}

/** fmtSize prints a size like the CLI does (523 B, 2.5 MB). */
export function fmtSize(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`
  return `${n} B`
}
