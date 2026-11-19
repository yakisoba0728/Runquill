// Sample responses for the Logs view: GET /api/runners/{name}/logs returns the
// runner container's output as plain text with Docker timestamps. The sample
// panel cannot stream, so every runner answers with the same fixed lines.
function logs(name, query) {
  const tail = Math.min(2000, Number(query.get("tail") ?? 200));
  const start = Date.now() - 180 * 1000;
  const stamp = (seconds) => new Date(start + seconds * 1000).toISOString().replace("Z", "000000Z");
  const job = name.replace(/^runquill-demo-/, "");
  const lines = [
    [0, "Runquill runner entrypoint: configuring ephemeral runner " + name],
    [1, "\u001b[1m√ Connected to GitHub\u001b[0m"],
    [2, ""],
    [2, "Current runner version: '2.329.0'"],
    [2, "2026-11-19 09:14:02Z: Listening for Jobs"],
    [4, "2026-11-19 09:14:04Z: Running job: build (job " + job + ")"],
    [5, "\u001b[32m[RUNNER 2026-11-19 09:14:05Z INFO JobDispatcher]\u001b[0m Successfully renew job request " + job + ", job is valid till 2026-11-19 09:24:05Z"],
    [35, "\u001b[32m[RUNNER 2026-11-19 09:14:35Z INFO JobDispatcher]\u001b[0m Successfully renew job request " + job + ", job is valid till 2026-11-19 09:24:35Z"],
    [65, "\u001b[33m[WORKER 2026-11-19 09:15:05Z WARN ProcessInvokerWrapper]\u001b[0m Process exited with code 0 after 59.8s"],
    [95, "\u001b[32m[RUNNER 2026-11-19 09:15:35Z INFO JobDispatcher]\u001b[0m Successfully renew job request " + job + ", job is valid till 2026-11-19 09:25:35Z"],
  ];
  return lines.slice(-tail || lines.length).map(([at, text]) => stamp(at) + " " + text).join("\n") + "\n";
}

// Fixture keys are exact paths, so answer any runner name.
export default new Proxy({}, {
  get(_, key) {
    const match = /^GET \/api\/runners\/([^/]+)\/logs$/.exec(String(key));
    return match ? (query) => logs(decodeURIComponent(match[1]), query) : undefined;
  },
});
