// loomcycle workspace kernel driver for Node.js (RFC GW §5.7).
//
// The workspace worker starts this file with `node` in a workspace and speaks
// to it in JSON lines: cells come in on stdin, frames go out on stdout. A
// cell runs through the V8 inspector's REPL mode on this thread, so `let` and
// `const` stay between cells and top-level `await` works. `require` resolves
// from the workspace; `await import()` does not work in that mode, so
// `loomImport(specifier)` is the helper for an ES module.
//
// A cell's output (process.stdout/stderr, console) is sent as out frames;
// nothing it writes through those reaches the protocol stream. A cell can
// still write to fd 1 itself and forge a frame: the supervisor treats every
// frame as untrusted, so that changes only its own result.
//
// Frames in:  {"type":"cell","id":ID,"code":CODE,"n":N,"max_output":BYTES,"max_value":BYTES}
// Frames out: {"type":"ready","protocol":1}
//             {"type":"out","id":ID,"text":TEXT}
//             {"type":"done","id":ID,"value":TEXT?,"error":TEXT?,"interrupted":BOOL?,"dropped":BYTES?}
//
// The supervisor interrupts a cell with SIGINT: code running on this thread
// is stopped by vm's breakOnSigint, a cell awaiting a promise is given up.
// It kills the process group when that does not end the cell. The driver
// exits when its stdin closes.
'use strict';

const fs = require('fs');
const path = require('path');
const util = require('util');
const vm = require('vm');
const url = require('url');
const { createRequire } = require('module');
const inspector = require('inspector');

const PROTOCOL = 1;
const PROTO_FD = 1;

function send(frame) {
  const data = Buffer.from(JSON.stringify(frame) + '\n', 'utf8');
  let off = 0;
  while (off < data.length) {
    try {
      off += fs.writeSync(PROTO_FD, data, off, data.length - off);
    } catch (e) {
      if (e.code !== 'EAGAIN') throw e;
    }
  }
}

function cut(text, limit) {
  const b = Buffer.from(text, 'utf8');
  if (b.length <= limit) return text;
  return b.subarray(0, limit).toString('utf8').replace(/�$/, '');
}

const root = process.cwd();
const workspaceRequire = createRequire(path.join(root, '__loom_cell__.js'));

// The cell's globals: require from the workspace, and the ES module helper.
globalThis.require = workspaceRequire;
globalThis.__loomInspect = util.inspect;
globalThis.loomImport = async function loomImport(specifier) {
  let target = specifier;
  try {
    const resolved = workspaceRequire.resolve(specifier);
    if (path.isAbsolute(resolved)) target = url.pathToFileURL(resolved).href;
  } catch (_) {
    // Not resolvable as CommonJS (an ESM-only package, a URL): let import try.
  }
  return import(target);
};

// The current cell, if any: its id and what its output may still take.
let cell = null;

function capture(chunk, encoding) {
  if (cell === null) return;
  let b = Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk), typeof encoding === 'string' ? encoding : 'utf8');
  const room = cell.maxOutput - cell.sent;
  if (room <= 0) {
    cell.dropped += b.length;
    return;
  }
  if (b.length > room) {
    cell.dropped += b.length - room;
    b = b.subarray(0, room);
  }
  cell.sent += b.length;
  const text = cell.decoder.write(b);
  if (text) send({ type: 'out', id: cell.id, text });
}

for (const stream of [process.stdout, process.stderr]) {
  stream.write = function write(chunk, encoding, cb) {
    capture(chunk, encoding);
    if (typeof encoding === 'function') encoding();
    else if (typeof cb === 'function') cb();
    return true;
  };
}

const session = new inspector.Session();
session.connect();

// Evaluated through vm so breakOnSigint covers the cell's synchronous part:
// the inspector runs a REPL-mode evaluation on this thread before post
// returns, and only its answer is delivered later.
globalThis.__loomEvaluate = function evaluate(expression, onAnswer) {
  session.post('Runtime.evaluate', {
    expression,
    replMode: true,
    awaitPromise: true,
    objectGroup: 'loom-cell',
  }, onAnswer);
};
const evaluateScript = new vm.Script('__loomEvaluate(__loomCell.code, __loomCell.onAnswer)', { filename: 'loom-driver' });

function describe(result, onText) {
  if (!result || result.type === 'undefined') return onText(undefined);
  if ('unserializableValue' in result) return onText(result.unserializableValue);
  if ('value' in result) return onText(util.inspect(result.value));
  if (!result.objectId) return onText(result.description);
  session.post('Runtime.callFunctionOn', {
    objectId: result.objectId,
    functionDeclaration: 'function () { return __loomInspect(this, { depth: 4 }); }',
    returnByValue: true,
  }, (err, r) => {
    if (err || !r || !r.result || typeof r.result.value !== 'string') return onText(result.description);
    onText(r.result.value);
  });
}

const INTERRUPTED = { interrupted: true, error: "the cell was interrupted; the kernel's variables are kept" };

// cellStack drops the frames below the cell: the driver's own, which say
// nothing about the cell and name where the driver lives.
function cellStack(text) {
  const lines = String(text).split('\n');
  const i = lines.findIndex((l) => l.includes('(node:inspector') || l.includes(__filename) || l.includes('loom-driver'));
  return (i > 0 ? lines.slice(0, i) : lines).join('\n') + '\n';
}

function finish(c, fields) {
  if (cell !== c) return;
  const tail = c.decoder.end();
  if (tail) send({ type: 'out', id: c.id, text: tail });
  cell = null;
  const done = Object.assign({ type: 'done', id: c.id }, fields);
  if (c.dropped > 0) done.dropped = c.dropped;
  session.post('Runtime.releaseObjectGroup', { objectGroup: 'loom-cell' }, () => {});
  send(done);
  if (queue.length > 0) setImmediate(() => runCell(queue.shift()));
}

const { StringDecoder } = require('string_decoder');

function runCell(frame) {
  const c = {
    id: String(frame.id || ''),
    maxOutput: Number(frame.max_output) || 0,
    maxValue: Number(frame.max_value) || 0,
    sent: 0,
    dropped: 0,
    decoder: new StringDecoder('utf8'),
  };
  cell = c;
  globalThis.__loomCell = {
    code: String(frame.code || ''),
    onAnswer: (err, r) => {
      if (cell !== c) return;
      if (err) {
        // breakOnSigint stopped the evaluation inside the inspector: the
        // termination comes back as the evaluation's error.
        if (/Execution was terminated/.test(String(err && err.message))) return finish(c, INTERRUPTED);
        return finish(c, { error: String(err && err.message || err) });
      }
      if (r.exceptionDetails) {
        const ex = r.exceptionDetails.exception;
        const text = (ex && ex.description) || r.exceptionDetails.text || 'the cell threw';
        return finish(c, { error: cut(cellStack(text), 64 * 1024) });
      }
      describe(r.result, (text) => finish(c, text === undefined ? {} : { value: cut(String(text), c.maxValue) }));
    },
  };
  try {
    evaluateScript.runInThisContext({ breakOnSigint: true });
  } catch (e) {
    finish(c, INTERRUPTED);
  }
}

// A SIGINT that arrives while the cell waits on a promise or a timer gives
// the cell up: what it awaited may still finish, and is then ignored. One
// that arrives between cells is ignored.
process.on('SIGINT', () => {
  if (cell !== null) {
    finish(cell, { interrupted: true, error: "the cell was interrupted while it waited; the kernel's variables are kept" });
  }
});

// A cell's own unhandled failure must not end the kernel.
process.on('uncaughtException', (e) => {
  capture(`Uncaught ${util.inspect(e)}\n`);
});
process.on('unhandledRejection', (e) => {
  capture(`Unhandled rejection: ${util.inspect(e)}\n`);
});

const queue = [];
let pending = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (data) => {
  pending += data;
  let i;
  while ((i = pending.indexOf('\n')) >= 0) {
    const line = pending.slice(0, i);
    pending = pending.slice(i + 1);
    let frame;
    try {
      frame = JSON.parse(line);
    } catch (_) {
      continue;
    }
    if (!frame || frame.type !== 'cell') continue;
    // One cell at a time: a cell sent while another still runs (one whose
    // done frame a cell forged) waits for it, as the Python driver's does.
    if (cell === null && queue.length === 0) runCell(frame);
    else queue.push(frame);
  }
});
process.stdin.on('end', () => process.exit(0));

send({ type: 'ready', protocol: PROTOCOL });
