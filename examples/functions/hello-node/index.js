// hello-node is the example Node.js function.
//
// Deploy:  beevm deploy hello-node examples/functions/hello-node
// Contract: BeeVM loads this module once per VM (and keeps it loaded inside the function
// snapshot), then calls handler(event) for every request. `event` is the request's JSON
// payload; the (awaited) return value must be JSON-serializable. console.log output becomes
// the function's log.
//
//   in:  {"name":"Om"}
//   out: {"greeting":"Hello, Om!","runtime":"node"}

exports.handler = async (event) => {
  const name = (event && event.name) || 'world';
  console.log('hello-node: greeting', name);
  return { greeting: `Hello, ${name}!`, runtime: 'node' };
};
