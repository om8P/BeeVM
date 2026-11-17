import { describe, expect, it } from 'vitest'
import { unzipSync, zipSync } from 'fflate'
import {
  DeployError,
  cleanPath,
  deployConfig,
  elfCheck,
  entriesError,
  folderFiles,
  handlerError,
  missingHandlerFile,
  nameError,
  prepare,
  runtimeFromNames,
  runtimeOfFile,
  settingError,
  stripRoot,
  suggestName,
  type SourceFile,
} from './deploy'

const text = (s: string) => new TextEncoder().encode(s)
const file = (path: string, s = 'x'): SourceFile => ({ path, data: text(s) })

// elf builds the start of a Linux program: a 64-byte header plus program headers.
// Defaults: 64-bit, little-endian, x86-64 (62), executable (2), one PT_LOAD (1).
function elf({ cls = 2, machine = 62, type = 2, phTypes = [1] } = {}): Uint8Array {
  const phoff = 64
  const phentsize = 56
  const b = new Uint8Array(phoff + phentsize * phTypes.length)
  b.set([0x7f, 0x45, 0x4c, 0x46, cls, 1])
  const v = new DataView(b.buffer)
  v.setUint16(16, type, true)
  v.setUint16(18, machine, true)
  v.setBigUint64(32, BigInt(phoff), true)
  v.setUint16(54, phentsize, true)
  v.setUint16(56, phTypes.length, true)
  phTypes.forEach((t, i) => v.setUint32(phoff + i * phentsize, t, true))
  return b
}

const names = (zip: Uint8Array) => Object.keys(unzipSync(zip)).sort()

describe('names', () => {
  it('follows the server rule', () => {
    expect(nameError('hello-python')).toBe('')
    expect(nameError('a')).toBe('')
    expect(nameError('')).toMatch(/name/)
    for (const bad of ['Hello', '-x', 'a_b', 'a.b', 'x'.repeat(64)]) expect(nameError(bad), bad).not.toBe('')
  })
  it('suggests a valid name from a file or folder name', () => {
    expect(suggestName('My App.py')).toBe('my-app')
    expect(suggestName('hello-node')).toBe('hello-node')
    expect(suggestName('pkg.zip')).toBe('pkg')
    expect(suggestName('___')).toBe('my-function')
    expect(nameError(suggestName('x'.repeat(80)))).toBe('')
  })
})

describe('handlers', () => {
  it('checks the syntax per runtime', () => {
    expect(handlerError('python', '')).toBe('')
    expect(handlerError('python', 'app.main')).toBe('')
    expect(handlerError('python', 'pkg.mod.fn')).toBe('')
    expect(handlerError('python', 'main')).not.toBe('')
    expect(handlerError('python', 'my-app.handler')).not.toBe('')
    expect(handlerError('node', 'lib/app.main')).toBe('')
    expect(handlerError('node', '../x.main')).not.toBe('')
    expect(handlerError('binary', 'x.y')).not.toBe('')
  })
  it('needs the handler file at the top of the package', () => {
    expect(missingHandlerFile('python', '', ['handler.py'])).toBe('')
    expect(missingHandlerFile('python', 'pkg.mod.fn', ['pkg/mod/__init__.py'])).toBe('')
    expect(missingHandlerFile('node', '', ['index.mjs'])).toBe('')
    expect(missingHandlerFile('python', '', ['src/handler.py'])).toMatch(/handler.py or handler\/__init__.py/)
  })
})

describe('detection', () => {
  it('finds the runtime from the names at the top', () => {
    expect(runtimeFromNames(['handler.py', 'lib/x.py'])).toBe('python')
    expect(runtimeFromNames(['index.cjs'])).toBe('node')
    expect(() => runtimeFromNames(['handler.py', 'index.js'])).toThrow(/both/)
    expect(() => runtimeFromNames(['src/handler.py'])).toThrow(DeployError)
  })
  it('tells files apart by name, then by content', () => {
    expect(runtimeOfFile('app.py', text(''))).toBe('python')
    expect(runtimeOfFile('app.MJS', text(''))).toBe('node')
    expect(runtimeOfFile('pkg', zipSync({ 'handler.py': text('') }))).toBe('zip')
    expect(runtimeOfFile('hello', elf())).toBe('binary')
    expect(() => runtimeOfFile('notes.txt', text('hi'))).toThrow(/Cannot tell/)
  })
})

describe('Linux programs', () => {
  it('accepts a static x86-64 program', () => {
    expect(elfCheck(elf())).toBe('')
    expect(elfCheck(elf({ type: 3 }))).toBe('') // position-independent, still static
  })
  it('warns about a dynamically linked one', () => {
    expect(elfCheck(elf({ phTypes: [6, 3, 1] }))).toMatch(/dynamically linked/)
  })
  it('rejects other files', () => {
    expect(() => elfCheck(text('#!/bin/sh\necho hi\n'))).toThrow(/not a Linux program/)
    expect(() => elfCheck(elf({ machine: 183 }))).toThrow(/another CPU/) // ARM64
    expect(() => elfCheck(elf({ cls: 1 }))).toThrow(/64-bit/)
    expect(() => elfCheck(elf({ type: 1 }))).toThrow(/not an executable/) // object file
  })
})

describe('package rules', () => {
  it('match the server', () => {
    expect(entriesError([{ name: 'handler.py', size: 10 }])).toBe('')
    expect(entriesError([])).toMatch(/empty/)
    expect(entriesError([{ name: '../evil', size: 1 }])).toMatch(/outside/)
    expect(entriesError([{ name: 'a/../../evil', size: 1 }])).toMatch(/outside/)
    expect(entriesError([{ name: '/etc/passwd', size: 1 }])).toMatch(/Bad file name/)
    expect(entriesError([{ name: 'a\\b', size: 1 }])).toMatch(/Bad file name/)
    expect(entriesError([{ name: 'big', size: 33 << 20 }])).toMatch(/32 MiB/)
    expect(entriesError(Array.from({ length: 2001 }, (_, i) => ({ name: `f${i}`, size: 1 })))).toMatch(/2001 files/)
  })
  it('cleans paths like Go', () => {
    expect(cleanPath('a/./b//c')).toBe('a/b/c')
    expect(cleanPath('a/../b')).toBe('b')
    expect(cleanPath('../a')).toBe('../a')
    expect(cleanPath('./')).toBe('.')
  })
  it('strips the chosen folder and skips what the CLI skips', () => {
    expect(stripRoot(['fn/handler.py', 'fn/lib/x.py'])).toEqual(['handler.py', 'lib/x.py'])
    expect(stripRoot(['handler.py', 'lib/x.py'])).toEqual(['handler.py', 'lib/x.py'])
    expect(stripRoot(['a/x', 'b/y'])).toEqual(['a/x', 'b/y'])
    const kept = folderFiles([file('fn/handler.py'), file('fn/.git/HEAD'), file('fn/__pycache__/x.pyc'), file('fn/node_modules/m.js')])
    expect(kept.map((f) => f.path)).toEqual(['fn/handler.py', 'fn/node_modules/m.js'])
  })
})

describe('settings and config', () => {
  it('checks the ranges', () => {
    expect(settingError({})).toBe('')
    expect(settingError({ memoryMiB: 256, vcpus: 2, timeoutS: 30 })).toBe('')
    expect(settingError({ memoryMiB: 32 })).toMatch(/Memory/)
    expect(settingError({ vcpus: 8 })).toMatch(/vCPUs/)
    expect(settingError({ timeoutS: 61 })).toMatch(/Timeout/)
    expect(settingError({ timeoutS: 1.5 })).toMatch(/Timeout/)
  })
  it('always sends the runtime, the rest only when set', () => {
    expect(deployConfig('binary', '', {})).toEqual({ runtime: 'binary' })
    expect(deployConfig('python', 'app.main', { memoryMiB: 256, timeoutS: 5 })).toEqual({
      runtime: 'python', handler: 'app.main', memory_mib: 256, timeout_ms: 5000,
    })
  })
})

describe('prepare', () => {
  it('zips one python file under its own name, handler <file>.handler (like the CLI)', async () => {
    const p = await prepare({ kind: 'file', file: file('app.py', 'def handler(e): return e') })
    expect(p.runtime).toBe('python')
    expect(p.handler).toBe('app.handler')
    expect(p.config).toEqual({ runtime: 'python', handler: 'app.handler' })
    expect(p.filename).toBe('app.zip')
    expect(names(p.code)).toEqual(['app.py'])
  })
  it('zips a folder with the files at the top level', async () => {
    const p = await prepare({
      kind: 'folder', name: 'hello-node',
      files: [file('hello-node/index.js'), file('hello-node/lib/util.js'), file('hello-node/.git/config')],
    })
    expect(p.runtime).toBe('node')
    expect(p.handler).toBe('index.handler')
    expect(p.config).toEqual({ runtime: 'node' }) // default handler: not sent
    expect(names(p.code)).toEqual(['index.js', 'lib/util.js'])
    expect(p.files).toBe(2)
  })
  it('sends a zip as it is, after checking it', async () => {
    const zip = zipSync({ 'handler.py': text('x'), 'lib/': new Uint8Array() })
    const p = await prepare({ kind: 'file', file: { path: 'pkg.zip', data: zip } })
    expect(p.runtime).toBe('python')
    expect(p.code).toBe(zip)
    await expect(prepare({ kind: 'file', file: { path: 'bad.zip', data: zipSync({ 'x.txt': text('x') }) } })).rejects.toThrow(/No handler.py/)
    await expect(
      prepare({ kind: 'file', file: { path: 'bad.zip', data: zipSync({ 'x.txt': text('x') }) } }, { runtime: 'python', handler: 'x.main' }),
    ).rejects.toThrow(/x.py/)
  })
  it('sends a static program as it is, and warns about a dynamic one', async () => {
    const p = await prepare({ kind: 'file', file: { path: 'hello', data: elf() } })
    expect(p).toMatchObject({ runtime: 'binary', handler: '', filename: 'hello', warnings: [], config: { runtime: 'binary' } })
    const d = await prepare({ kind: 'file', file: { path: 'hello', data: elf({ phTypes: [3] }) } })
    expect(d.warnings[0]).toMatch(/dynamically linked/)
  })
  it('stops on broken rules with a message', async () => {
    await expect(prepare({ kind: 'file', file: file('hello') })).rejects.toThrow(/Cannot tell/)
    await expect(prepare({ kind: 'file', file: { path: 'hello', data: elf() } }, { handler: 'x.y' })).rejects.toThrow(/no handler/)
    await expect(prepare({ kind: 'file', file: file('my-app.py') })).rejects.toThrow(/module.function/)
    await expect(prepare({ kind: 'folder', name: 'fn', files: [file('fn/index.js')] }, { runtime: 'binary' })).rejects.toThrow(/folder cannot be a binary/)
    await expect(prepare({ kind: 'folder', name: 'fn', files: [file('fn/.git/HEAD')] })).rejects.toThrow(/no files/)
  })
  it('lets the user pick the runtime when both are present', async () => {
    const src = { kind: 'folder' as const, name: 'fn', files: [file('fn/handler.py'), file('fn/index.js')] }
    await expect(prepare(src)).rejects.toThrow(/both/)
    expect((await prepare(src, { runtime: 'node' })).runtime).toBe('node')
  })
})
