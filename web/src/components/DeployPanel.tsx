import { useEffect, useRef, useState, type DragEvent } from 'react'
import { ApiError, type Client, type Fn } from '../api'
import {
  DeployError,
  MEMORY_MIB,
  TIMEOUT_S,
  VCPUS,
  fmtSize,
  nameError,
  prepare,
  settingError,
  suggestName,
  type Overrides,
  type Prepared,
  type Runtime,
  type Source,
  type SourceFile,
} from '../deploy'

interface Props {
  client: Client
  functions: Fn[]
  onError: (e: unknown) => void
  onTest: (name: string) => void // open the Invoke panel on this function
}

// Folders the CLI never packs; skipped while reading a dropped folder so .git is not read at all.
const SKIP = new Set(['.git', '__pycache__'])

// DeployPanel: upload a function (create or replace), like `beevm deploy`.
export function DeployPanel({ client, functions, onError, onTest }: Props) {
  const [src, setSrc] = useState<Source | null>(null)
  const [label, setLabel] = useState('')
  const [name, setName] = useState('')
  const [nameEdited, setNameEdited] = useState(false)
  const [runtime, setRuntime] = useState<Runtime | ''>('')
  const [handler, setHandler] = useState('')
  const [memory, setMemory] = useState('')
  const [vcpus, setVcpus] = useState('')
  const [timeout, setTimeout_] = useState('')
  const [preview, setPreview] = useState<Prepared | null>(null)
  const [previewErr, setPreviewErr] = useState('')
  const [over, setOver] = useState(false)
  const [busy, setBusy] = useState(false)
  const [progress, setProgress] = useState(0)
  const [done, setDone] = useState<{ text: string; name: string; warnings: string[] } | null>(null)
  const [err, setErr] = useState('')
  const fileInput = useRef<HTMLInputElement>(null)
  const folderInput = useRef<HTMLInputElement>(null)

  // React has no prop for folder pickers; the attribute works in every current browser.
  useEffect(() => {
    folderInput.current?.setAttribute('webkitdirectory', '')
  }, [])

  const num = (s: string) => (s.trim() === '' ? undefined : Number(s))
  const overrides: Overrides = {
    runtime,
    handler,
    memoryMiB: num(memory),
    vcpus: num(vcpus),
    timeoutS: num(timeout),
  }

  // Detect the runtime and check the rules as soon as something is picked (and again when the
  // runtime or handler changes), so problems show before pressing Deploy.
  useEffect(() => {
    if (!src) return
    let stale = false
    setPreviewErr('')
    prepare(src, { runtime, handler }).then(
      (p) => !stale && setPreview(p),
      (e) => {
        if (stale) return
        setPreview(null)
        setPreviewErr(e instanceof Error ? e.message : String(e))
      },
    )
    return () => {
      stale = true
    }
  }, [src, runtime, handler])

  const choose = (s: Source, what: string) => {
    setSrc(s)
    setLabel(what)
    setDone(null)
    setErr('')
    if (!nameEdited) setName(suggestName(s.kind === 'file' ? s.file.path : s.name))
  }

  const pickFile = async (files: FileList | null) => {
    const f = files?.[0]
    if (!f) return
    choose({ kind: 'file', file: { path: f.name, data: new Uint8Array(await f.arrayBuffer()) } }, `${f.name} (${fmtSize(f.size)})`)
  }

  const pickFolder = async (files: FileList | null) => {
    if (!files || files.length === 0) return
    const list = Array.from(files).filter((f) => !f.webkitRelativePath.split('/').some((p) => SKIP.has(p)))
    const read = await Promise.all(list.map(async (f) => ({ path: f.webkitRelativePath, data: new Uint8Array(await f.arrayBuffer()) })))
    const folder = files[0].webkitRelativePath.split('/')[0]
    choose({ kind: 'folder', name: folder, files: read }, `${folder}/ (folder, ${read.length} files)`)
  }

  const onDrop = async (e: DragEvent) => {
    e.preventDefault()
    setOver(false)
    const items = Array.from(e.dataTransfer.items).filter((i) => i.kind === 'file')
    if (items.length !== 1) {
      setErr('Drop one file or one folder.')
      return
    }
    const entry = items[0].webkitGetAsEntry()
    try {
      if (entry?.isDirectory) {
        const files: SourceFile[] = []
        await readFolder(entry as FileSystemDirectoryEntry, `${entry.name}/`, files)
        choose({ kind: 'folder', name: entry.name, files }, `${entry.name}/ (folder, ${files.length} files)`)
      } else {
        await pickFile(e.dataTransfer.files)
      }
    } catch (x) {
      setErr(`Could not read what was dropped: ${x instanceof Error ? x.message : String(x)}`)
    }
  }

  const nErr = name ? nameError(name) : ''
  const sErr = settingError(overrides)
  const exists = functions.some((f) => f.name === name)
  const canDeploy = !!src && !!preview && !nErr && !sErr && name !== '' && !busy

  const deploy = async () => {
    if (!src) return
    if (exists && !window.confirm(`Replace the function ${name}? Its warm VMs and snapshot are thrown away.`)) return
    setBusy(true)
    setErr('')
    setDone(null)
    setProgress(0)
    try {
      const p = await prepare(src, overrides)
      const r = await client.deploy(name, p.code, p.filename, p.config, (sent, total) => setProgress(total ? sent / total : 0))
      const f = r.fn
      const kind = f.handler ? `${f.runtime} ${f.handler}` : f.runtime
      setDone({
        name: f.name,
        warnings: p.warnings,
        text: `${r.created ? 'Created' : 'Updated'} ${f.name} (${kind}): ${fmtSize(f.code_size)}, ${f.memory_mib} MiB, ${f.vcpus} vCPU, timeout ${f.timeout_ms / 1000}s.`,
      })
    } catch (e) {
      onError(e)
      setErr(describe(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="card panel deploy" aria-label="Deploy">
      <div className="panel-head">
        <h2>Deploy</h2>
        <span className="muted small">create or replace a function</span>
      </div>

      <div
        className={`drop${over ? ' over' : ''}`}
        onDragOver={(e) => {
          e.preventDefault()
          setOver(true)
        }}
        onDragLeave={() => setOver(false)}
        onDrop={onDrop}
      >
        <p className="drop-text">Drop a .py or .js file, a .zip, a folder or a Linux program here</p>
        <div className="drop-buttons">
          <button type="button" className="ghost small-btn" onClick={() => fileInput.current?.click()}>
            Choose file
          </button>
          <button type="button" className="ghost small-btn" onClick={() => folderInput.current?.click()}>
            Choose folder
          </button>
        </div>
        <input ref={fileInput} type="file" hidden onChange={(e) => pickFile(e.target.files).finally(() => (e.target.value = ''))} />
        <input ref={folderInput} type="file" hidden multiple onChange={(e) => pickFolder(e.target.files).finally(() => (e.target.value = ''))} />
        {label && <p className="drop-picked small">{label}</p>}
      </div>

      {src && previewErr && (
        <p className="error" role="alert">
          {previewErr}
        </p>
      )}
      {src && preview && (
        <p className="detected small">
          <strong>{preview.runtime}</strong>
          {runtime ? '' : ' (detected)'}
          {preview.handler && (
            <>
              {' '}
              · handler <code>{preview.handler}</code>
            </>
          )}{' '}
          · {preview.runtime === 'binary' ? fmtSize(preview.code.length) : `${preview.files} file${preview.files === 1 ? '' : 's'}, ${fmtSize(preview.code.length)} zipped`}
        </p>
      )}
      {preview?.warnings.map((w) => (
        <p key={w} className="warn small">
          {w}
        </p>
      ))}

      <label htmlFor="deploy-name">Function name</label>
      <input
        id="deploy-name"
        value={name}
        spellCheck={false}
        autoComplete="off"
        placeholder="hello-python"
        onChange={(e) => {
          setName(e.target.value)
          setNameEdited(true)
        }}
        aria-invalid={!!nErr}
        aria-describedby="deploy-name-help"
      />
      <p id="deploy-name-help" className={`small ${nErr ? 'error' : 'muted'} field-help`}>
        {nErr || (exists ? 'A function with this name exists: deploying replaces it.' : 'Lowercase letters, digits and "-".')}
      </p>

      <details className="more">
        <summary>Settings</summary>
        <div className="settings">
          <div>
            <label htmlFor="deploy-runtime">Runtime</label>
            <select id="deploy-runtime" value={runtime} onChange={(e) => setRuntime(e.target.value as Runtime | '')}>
              <option value="">detect</option>
              <option value="python">python</option>
              <option value="node">node</option>
              <option value="binary">binary</option>
            </select>
          </div>
          <div>
            <label htmlFor="deploy-handler">Handler</label>
            <input
              id="deploy-handler"
              value={handler}
              spellCheck={false}
              placeholder={preview?.handler || 'default'}
              disabled={preview?.runtime === 'binary'}
              onChange={(e) => setHandler(e.target.value)}
            />
          </div>
          <div>
            <label htmlFor="deploy-mem">Memory (MiB)</label>
            <input id="deploy-mem" type="number" inputMode="numeric" min={MEMORY_MIB.min} max={MEMORY_MIB.max} step={64} placeholder={String(MEMORY_MIB.def)} value={memory} onChange={(e) => setMemory(e.target.value)} />
          </div>
          <div>
            <label htmlFor="deploy-vcpus">vCPUs</label>
            <input id="deploy-vcpus" type="number" inputMode="numeric" min={VCPUS.min} max={VCPUS.max} placeholder={String(VCPUS.def)} value={vcpus} onChange={(e) => setVcpus(e.target.value)} />
          </div>
          <div>
            <label htmlFor="deploy-timeout">Timeout (s)</label>
            <input id="deploy-timeout" type="number" inputMode="numeric" min={TIMEOUT_S.min} max={TIMEOUT_S.max} placeholder={String(TIMEOUT_S.def)} value={timeout} onChange={(e) => setTimeout_(e.target.value)} />
          </div>
        </div>
        {sErr && <p className="error small">{sErr}</p>}
      </details>

      <button className="deploy-btn" onClick={deploy} disabled={!canDeploy}>
        {busy ? 'Deploying…' : exists ? 'Deploy (replace)' : 'Deploy'}
      </button>

      {busy && (
        <div className="meter" role="progressbar" aria-label="Upload" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(progress * 100)}>
          <div className="meter-fill" style={{ width: `${Math.round(progress * 100)}%` }} />
        </div>
      )}

      {err && (
        <p className="error" role="alert">
          {err}
        </p>
      )}

      {done && (
        <div className="result deploy-done" role="status">
          <p>
            <span className="badge ok">ok</span> {done.text}
          </p>
          <p className="muted small">Its function snapshot is built in the background now, so the very first call may be a little slower.</p>
          <button className="small-btn" onClick={() => onTest(done.name)}>
            Test it
          </button>
        </div>
      )}
    </section>
  )
}

// readFolder collects a dropped folder's files, paths starting with prefix ("folder/...").
async function readFolder(dir: FileSystemDirectoryEntry, prefix: string, out: SourceFile[]): Promise<void> {
  const reader = dir.createReader()
  // readEntries returns the entries in batches; an empty batch means the end.
  for (;;) {
    const batch = await new Promise<FileSystemEntry[]>((res, rej) => reader.readEntries(res, rej))
    if (batch.length === 0) return
    for (const e of batch) {
      if (SKIP.has(e.name)) continue
      if (e.isDirectory) {
        await readFolder(e as FileSystemDirectoryEntry, `${prefix}${e.name}/`, out)
      } else if (e.isFile) {
        const f = await new Promise<File>((res, rej) => (e as FileSystemFileEntry).file(res, rej))
        out.push({ path: prefix + e.name, data: new Uint8Array(await f.arrayBuffer()) })
      }
    }
  }
}

// describe turns a failed deploy into one sentence for the user.
function describe(e: unknown): string {
  if (e instanceof DeployError) return e.message
  if (e instanceof ApiError) {
    switch (e.status) {
      case 0:
        return 'Could not reach the server. Is beevmd serve running?'
      case 413:
        return 'The code is larger than the server accepts (50 MiB).'
      case 503:
        return `The server cannot store code right now: ${e.message}`
    }
    return `${e.status}: ${e.message}`
  }
  return String(e)
}
