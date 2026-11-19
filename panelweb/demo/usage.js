// Sample responses for the Usage view: the same shape as GET /api/usage.
// Jobs are generated deterministically per UTC day, so a range always shows
// the same numbers, and every group adds up to the same totals.
const repositories = [
  { name: "garden-lab/papertrail", pool: "default", hosts: ["local"], perDay: 14 },
  { name: "garden-lab/pattern-kit", pool: "build", hosts: ["builder-west"], perDay: 9 },
  { name: "garden-lab/forecast", pool: "default", hosts: ["local", "builder-west"], perDay: 6 },
  { name: "garden-lab/first-workflow", pool: "default", hosts: ["local"], perDay: 2 },
];

function random(seed) {
  let x = seed | 0;
  return () => {
    x = (x + 0x6d2b79f5) | 0;
    let t = Math.imul(x ^ (x >>> 15), 1 | x);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function jobsOn(day) {
  const index = Math.floor(Date.parse(day + "T00:00:00Z") / 86400000);
  const weekend = [0, 6].includes(new Date(index * 86400000).getUTCDay());
  const jobs = [];
  repositories.forEach((repository, r) => {
    const next = random(index * 31 + r);
    const count = Math.round(repository.perDay * (weekend ? 0.3 : 1) * (0.6 + next() * 0.8));
    for (let i = 0; i < count; i++) {
      const failed = next() < 0.06;
      const neverStarted = failed && next() < 0.5;
      const wait = neverStarted ? 300 + Math.round(next() * 3000) : Math.round(2 + next() ** 3 * 240);
      jobs.push({
        day, repository: repository.name, pool: repository.pool, failed,
        host: neverStarted ? "" : repository.hosts[Math.floor(next() * repository.hosts.length)],
        runner: neverStarted ? 0 : Math.round(60 + next() * 900), wait,
      });
    }
  });
  return jobs;
}

function percentile(sorted, p) {
  if (!sorted.length) return 0;
  return sorted[Math.min(sorted.length, Math.max(1, Math.ceil(p * sorted.length))) - 1];
}

function totals(jobs) {
  const waits = jobs.map((job) => job.wait).sort((a, b) => a - b);
  const sum = waits.reduce((total, wait) => total + wait, 0);
  return {
    jobs: jobs.length,
    failed: jobs.filter((job) => job.failed).length,
    runner_seconds: jobs.reduce((total, job) => total + job.runner, 0),
    wait_count: waits.length,
    wait_avg_seconds: waits.length ? Math.floor(sum / waits.length) : 0,
    wait_p50_seconds: percentile(waits, 0.5),
    wait_p95_seconds: percentile(waits, 0.95),
  };
}

function usage(query) {
  const today = new Date().toISOString().slice(0, 10);
  const to = query.get("to") || today;
  const fallback = new Date(Date.parse(to + "T00:00:00Z") - 29 * 86400000).toISOString().slice(0, 10);
  const from = query.get("from") || fallback;
  const group = query.get("group") || "day";
  const repository = (query.get("repository") || "").trim().toLowerCase();
  const days = [];
  for (let at = Date.parse(from + "T00:00:00Z"); at <= Date.parse(to + "T00:00:00Z") && days.length < 400; at += 86400000) {
    days.push(new Date(at).toISOString().slice(0, 10));
  }
  const jobs = days.filter((day) => day <= today).flatMap(jobsOn)
    .filter((job) => !repository || job.repository === repository);
  const groups = new Map(group === "day" ? days.map((day) => [day, []]) : []);
  for (const job of jobs) {
    const key = job[group];
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(job);
  }
  const series = [...groups].map(([key, list]) => ({ key, ...totals(list) }));
  if (group === "day") series.sort((a, b) => a.key.localeCompare(b.key));
  else series.sort((a, b) => b.jobs - a.jobs || a.key.localeCompare(b.key));
  return { from, to, group, totals: totals(jobs), series };
}

export default { "GET /api/usage": usage };
