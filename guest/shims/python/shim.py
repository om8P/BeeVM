"""BeeVM Python runtime shim.

The guest agent starts this once per VM and keeps it running, so the interpreter and the
user's module stay loaded between calls (and inside snapshots). It talks newline-delimited
JSON with the agent on two extra file descriptors, so the user's own prints can never mix with
the protocol:

    fd 3  requests from the agent   {"op": "load", "dir": "...", "handler": "module.func"}
                                    {"op": "invoke", "payload": <any JSON>}
    fd 4  answers to the agent      {"ok": true, "output": <JSON>, "logs": "..."}
                                    {"ok": false, "error": "...", "logs": "..."}

A load with an empty handler only answers "ok": that is the runtime snapshot step (interpreter
started, nothing imported yet). The handler is called as handler(event) or, if it takes two
parameters, handler(event, context) like AWS Lambda.
"""

import contextlib
import importlib
import inspect
import io
import json
import os
import sys
import time
import traceback

MAX_LOGS = 64 * 1024  # keep the last 64 KB of a call's prints, like the agent's stderr limit

requests = os.fdopen(3, "r", encoding="utf-8")
answers = os.fdopen(4, "w", encoding="utf-8")

handler = None
handler_takes_context = False
handler_name = ""


def reply(msg):
    """msg is a dict, or an answer that is already JSON text."""
    if not isinstance(msg, str):
        msg = json.dumps(msg, separators=(",", ":"))
    answers.write(msg + "\n")
    answers.flush()


def tail(text):
    return text[-MAX_LOGS:] if len(text) > MAX_LOGS else text


def load(req):
    global handler, handler_takes_context, handler_name
    name = req.get("handler", "")
    if not name:
        return {"ok": True}  # interpreter only
    module_name, _, func_name = name.rpartition(".")
    if not module_name or not func_name:
        return {"ok": False, "error": f"handler {name!r} must look like module.function"}
    code_dir = req.get("dir", "")
    if code_dir and code_dir not in sys.path:
        sys.path.insert(0, code_dir)
    logs = io.StringIO()
    try:
        with contextlib.redirect_stdout(logs), contextlib.redirect_stderr(logs):
            module = importlib.import_module(module_name)
        func = getattr(module, func_name, None)
        if not callable(func):
            return {"ok": False, "error": f"{module_name} has no function {func_name!r}", "logs": tail(logs.getvalue())}
    except BaseException:  # SyntaxError, ImportError, a SystemExit at import time, ...
        return {"ok": False, "error": traceback.format_exc(), "logs": tail(logs.getvalue())}
    try:
        params = [p for p in inspect.signature(func).parameters.values()
                  if p.kind in (p.POSITIONAL_ONLY, p.POSITIONAL_OR_KEYWORD)]
        handler_takes_context = len(params) >= 2
    except (TypeError, ValueError):
        handler_takes_context = False
    handler, handler_name = func, name
    return {"ok": True, "logs": tail(logs.getvalue())}


def invoke(req):
    if handler is None:
        return {"ok": False, "error": "no handler loaded"}
    event = req.get("payload")
    logs = io.StringIO()
    try:
        with contextlib.redirect_stdout(logs), contextlib.redirect_stderr(logs):
            if handler_takes_context:
                context = {"function": handler_name, "start_unix_ms": int(time.time() * 1000)}
                result = handler(event, context)
            else:
                result = handler(event)
        # allow_nan=False: NaN/Infinity are not valid JSON; make them a clear error instead.
        output = json.dumps(result, separators=(",", ":"), allow_nan=False)
    except BaseException:
        return {"ok": False, "error": traceback.format_exc(), "logs": tail(logs.getvalue())}
    # The output is already JSON text: put it into the answer line as it is instead of
    # decoding it and encoding it again (outputs can be megabytes).
    return '{"ok":true,"output":' + output + ',"logs":' + json.dumps(tail(logs.getvalue())) + "}"


def main():
    for line in requests:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
            op = req.get("op")
            if op == "load":
                reply(load(req))
            elif op == "invoke":
                reply(invoke(req))
            else:
                reply({"ok": False, "error": f"unknown op {op!r}"})
        except Exception:
            reply({"ok": False, "error": "shim: " + traceback.format_exc()})


if __name__ == "__main__":
    main()
