// Sample responses for the Usage view: the same shapes as GET /api/usage and GET /api/usage/rightsizing.
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


// GET /api/usage/rightsizing: the same shape as job_resources.go readRightsizing.
const resource = (limit, peak, ratio, verdict) => ({ limit, peak_max: peak, ...(ratio == null ? {} : { peak_ratio: ratio }), verdict });
function rightsizing(query) {
  const group = query.get("group") === "job" ? "job" : "pool";
  const days = Math.min(7, Math.max(1, Number(query.get("days")) || 7));
  const scale = (n) => Math.max(1, Math.round(n * days / 7));
  const groups = group === "pool" ? [
    { key: "default", jobs: scale(148), sampled_jobs: scale(131), oom_kills: 2,
      cpu: resource(2, 1.97, 0.99, "tight"), memory: resource(4096, 4096, 1, "tight") },
    { key: "build", jobs: scale(61), sampled_jobs: 0, oom_kills: 0 },
  ] : [
    { key: "garden-lab/papertrail runquill-18062-1-test", jobs: scale(90), sampled_jobs: scale(84), oom_kills: 0,
      cpu: resource(2, 0.38, 0.19, "oversized"), memory: resource(4096, 702, 0.17, "oversized") },
    { key: "garden-lab/forecast runquill-18060-1-forecast", jobs: scale(40), sampled_jobs: scale(35), oom_kills: 2,
      cpu: resource(2, 1.97, 0.99, "tight"), memory: resource(4096, 4096, 1, "tight") },
    { key: "garden-lab/pattern-kit runquill-18061-1-os__ubuntu2204__lint", jobs: scale(61), sampled_jobs: 0, oom_kills: 0 },
    { key: "garden-lab/first-workflow runquill-17990-1-first-job", jobs: 2, sampled_jobs: 2, oom_kills: 0,
      cpu: resource(2, 0.41, 0.21, "too_few_jobs"), memory: resource(0, 612, null, "unlimited") },
  ];
  return { group, days, groups };
}

export default { "GET /api/usage": usage, "GET /api/usage/rightsizing": rightsizing };
