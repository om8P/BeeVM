'use strict';
// BeeVM Node.js runtime shim.
//
// The guest agent starts this once per VM and keeps it running, so V8 and the user's module
// stay loaded between calls (and inside snapshots). It talks newline-delimited JSON with the
// agent on two extra file descriptors, so the user's own console.log output can never mix
// with the protocol:
//
//   fd 3  requests from the agent   {"op":"load","dir":"...","handler":"index.handler"}
//                                   {"op":"invoke","payload":<any JSON>}
//   fd 4  answers to the agent      {"ok":true,"output":<JSON>,"logs":"..."}
//                                   {"ok":false,"error":"...","logs":"..."}
//
// A load with an empty handler only answers "ok": that is the runtime snapshot step (V8
// started, nothing imported yet). The handler is called as handler(event) or, if it declares
// two parameters, handler(event, context) like AWS Lambda; it may be async.

const fs = require('fs');
const path = require('path');
const readline = require('readline');
const { pathToFileURL } = require('url');

const MAX_LOGS = 64 * 1024; // keep the last 64 KB of a call's output, like the agent's stderr limit
const EXTENSIONS = ['.js', '.mjs', '.cjs'];

let handler = null;
let handlerName = '';

function reply(line) {
  // Synchronous write: answers leave in order and before the next request is read.
  fs.writeSync(4, line + '\n');
}

function errorText(err) {
  if (err && err.stack) return String(err.stack);
  return String(err);
}

// Capture everything written to stdout/stderr (console.log and friends) while fn runs.
async function captured(fn) {
  const chunks = [];
  let size = 0;
  const grab = (chunk, encoding, cb) => {
    const text = typeof chunk === 'string' ? chunk : Buffer.from(chunk).toString('utf8');
    chunks.push(text);
    size += text.length;
    while (size > MAX_LOGS * 2 && chunks.length > 1) size -= chunks.shift().length;
    if (typeof encoding === 'function') encoding();
    else if (typeof cb === 'function') cb();
    return true;
  };
  const out = process.stdout.write;
  const err = process.stderr.write;
  process.stdout.write = grab;
  process.stderr.write = grab;
  try {
    const value = await fn();
    return { value, logs: tail(chunks.join('')) };
  } catch (e) {
    e.beevmLogs = tail(chunks.join(''));
    throw e;
  } finally {
    process.stdout.write = out;
    process.stderr.write = err;
  }
}

function tail(text) {
  return text.length > MAX_LOGS ? text.slice(-MAX_LOGS) : text;
}

function resolveModule(dir, moduleName) {
  const base = path.resolve(dir, moduleName);
  for (const ext of EXTENSIONS) {
    if (fs.existsSync(base + ext)) return base + ext;
  }
  return null;
}

async function load(req) {
  const name = req.handler || '';
  if (!name) return { ok: true }; // interpreter only
  const dot = name.lastIndexOf('.');
  if (dot <= 0 || dot === name.length - 1) {
    return { ok: false, error: `handler ${JSON.stringify(name)} must look like file.function` };
  }
  const moduleName = name.slice(0, dot);
  const funcName = name.slice(dot + 1);
  const file = resolveModule(req.dir || '.', moduleName);
  if (!file) {
    return { ok: false, error: `no ${moduleName}.js, .mjs or .cjs in the package` };
  }
  try {
    const { value: mod, logs } = await captured(() => import(pathToFileURL(file).href));
    // CommonJS exports show up on `default`; ES modules export names directly.
    let fn = mod[funcName];
    if (typeof fn !== 'function' && mod.default) fn = mod.default[funcName];
    if (typeof fn !== 'function') {
      return { ok: false, error: `${path.basename(file)} does not export a function ${JSON.stringify(funcName)}`, logs };
    }
    handler = fn;
    handlerName = name;
    return { ok: true, logs };
  } catch (e) {
    return { ok: false, error: errorText(e), logs: e.beevmLogs || '' };
  }
}

async function invoke(req) {
  if (!handler) return JSON.stringify({ ok: false, error: 'no handler loaded' });
  const event = req.payload === undefined ? null : req.payload;
  try {
    const { value, logs } = await captured(() =>
      handler.length >= 2
        ? handler(event, { function: handlerName, startUnixMs: Date.now() })
        : handler(event));
    let output = JSON.stringify(value);
    if (output === undefined) output = 'null'; // undefined or a function: no value
    // The output is already JSON text: put it into the answer line as it is.
    return '{"ok":true,"output":' + output + ',"logs":' + JSON.stringify(logs) + '}';
  } catch (e) {
    return JSON.stringify({ ok: false, error: errorText(e), logs: e.beevmLogs || '' });
  }
}

async function main() {
  const lines = readline.createInterface({ input: fs.createReadStream(null, { fd: 3 }), crlfDelay: Infinity });
  for await (const raw of lines) {
    const line = raw.trim();
    if (!line) continue;
    try {
      const req = JSON.parse(line);
      if (req.op === 'load') reply(JSON.stringify(await load(req)));
      else if (req.op === 'invoke') reply(await invoke(req));
      else reply(JSON.stringify({ ok: false, error: `unknown op ${JSON.stringify(req.op)}` }));
    } catch (e) {
      reply(JSON.stringify({ ok: false, error: 'shim: ' + errorText(e) }));
    }
  }
}

// A rejected promise nobody awaited would otherwise only warn; treat it like a crash so a
// broken VM is not reused. The agent sees the exit and reports "runtime exited".
process.on('unhandledRejection', (e) => {
  fs.writeSync(2, 'unhandled rejection: ' + errorText(e) + '\n');
  process.exit(1);
});

main().catch((e) => {
  fs.writeSync(2, 'shim failed: ' + errorText(e) + '\n');
  process.exit(1);
});
