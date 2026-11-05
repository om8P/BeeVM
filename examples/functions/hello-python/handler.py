"""hello-python is the example Python function.

Deploy:  beevm deploy hello-python examples/functions/hello-python
Contract: BeeVM imports this module once per VM (and keeps it imported inside the function
snapshot), then calls handler(event) for every request. `event` is the request's JSON payload;
the return value must be JSON-serializable. Anything printed becomes the function's log.

    in:  {"name":"Om"}
    out: {"greeting":"Hello, Om!","runtime":"python"}
"""


def handler(event):
    name = (event or {}).get("name") or "world"
    print("hello-python: greeting", name)
    return {"greeting": f"Hello, {name}!", "runtime": "python"}
