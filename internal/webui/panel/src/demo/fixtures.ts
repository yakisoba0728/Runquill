// Sample answers for the screens' GET requests, the same shapes as the server's (ported from panelweb/demo/*.js).
// Built into demo-* only, like the rest of src/demo. answer() is registered with api.ts by main.tsx.
import type { ElasticReport, HistoryJob, JobResources, ResourceReport, RightsizingReport, UsageReport } from "../types";

const at = (seconds: number) => new Date(Date.now() - seconds * 1000).toISOString();

// GET /api/jobs and /api/jobs/{id}.
function rows(): HistoryJob[] {
  const row = (id: number, repository: string, outcome: string, extra: Partial<HistoryJob> = {}): HistoryJob => ({
    id, repository: "garden-lab/" + repository, outcome, runner_label: "runquill-18062-1-" + repository,
    url: "https://github.com/garden-lab/" + repository + "/actions/runs/18062/job/" + id, ...extra,
  });
  return [
    row(4828, "pattern-kit", "waiting", { pool: "build", runner_os: "ubuntu2004", queued_at: at(58), wait_seconds: 58,
      runner_label: "runquill-18062-1-os__ubuntu2004__check", retry_at: at(-12),
      retry_reason: 'select runner image: runner image "ubuntu2004" is not in the runner image catalogue; add it to RUNQUILL_RUNNER_IMAGES' }),
    row(4826, "papertrail", "running", { pool: "default", host: "local", queued_at: at(119), started_at: at(110), wait_seconds: 5 }),
    row(4824, "pattern-kit", "running", { pool: "build", runner_os: "ubuntu2204", host: "builder-west",
      runner_label: "runquill-18061-1-os__ubuntu2204__lint", queued_at: at(221), started_at: at(213), wait_seconds: 4 }),
    row(4823, "forecast", "finished", { pool: "default", host: "local", queued_at: at(2300), started_at: at(2290),
      finished_at: at(1720), cleaned_at: at(1716), wait_seconds: 4 }),
    row(4821, "forecast", "failed", { pool: "default", queued_at: at(90000), finished_at: at(3600), wait_seconds: 86400,
      failure: "not started within 24 hours" }),
    row(4819, "pattern-kit", "failed", { pool: "build", queued_at: at(5400), finished_at: at(5100), wait_seconds: 300,
      failure: "cancelled by @garden-admin" }),
    row(4817, "first-workflow", "finished", { pool: "default", host: "local", queued_at: at(506), started_at: at(494),
      finished_at: at(479), cleaned_at: at(475), wait_seconds: 6 }),
  ];
}

function history(query: URLSearchParams) {
  const outcome = query.get("outcome");
  const repository = query.get("repository")?.toLowerCase();
  const since = Number(query.get("since")) * 1000;
  return {
    jobs: rows().filter((job) => (!outcome || job.outcome === outcome) && (!repository || job.repository === repository) &&
      (!since || Date.parse(job.queued_at!) >= since)),
  };
}

// Seconds before now of each recorded moment: created, discovered, provisioned, started, completed.
const timings: Record<number, number[]> = {
  4817: [510, 506, 500, 494, 479], 4823: [2302, 2300, 2296, 2290, 1720], 4824: [221, 220, 217, 213],
  4825: [900, 899, 896, 890, 24], 4826: [120, 119, 114, 110],
};

const resources: Record<number, JobResources> = {
  4817: { samples: 3, cpu_limit: 2, memory_limit_mib: 4096, cpu_peak: 0.41, cpu_average: 0.22, memory_peak_mib: 612, oom_killed: false },
  4823: { samples: 57, cpu_limit: 2, memory_limit_mib: 4096, cpu_limit_max: 2, memory_limit_max_mib: 4096,
    cpu_peak: 1.97, cpu_average: 1.41, memory_peak_mib: 4096, oom_killed: true },
  4825: { samples: 87, cpu_limit: 2, memory_limit_mib: 4096, cpu_peak: 1.12, cpu_average: 0.74, memory_peak_mib: 4096, oom_killed: true },
  4826: { samples: 11, cpu_limit: 2, memory_limit_mib: 4096, cpu_limit_max: 3, memory_limit_max_mib: 5120,
    cpu_peak: 2.71, cpu_average: 1.62, memory_peak_mib: 4710 },
};

// Every job in the sample snapshots has a record, as on a live panel; any other id is not found.
const known = [4817, 4819, 4821, 4823, 4824, 4825, 4826, 4827, 4828, 4831, 4835, 4836, 4838, 4840];

function detail(id: number) {
  if (!known.includes(id)) return undefined;
  const job = rows().find((candidate) => candidate.id === id) ?? { id, repository: "garden-lab/sample", outcome: "running" };
  const moments = timings[id];
  const [created, found, provisioned, started, completed] = moments ?? [];
  return {
    ...job,
    timing: moments && {
      discovery_source: "webhook", github_created_at: at(created), discovered_at: at(found), provisioned_at: at(provisioned),
      github_started_at: at(started), creation_to_discovery_seconds: created - found, runquill_queue_seconds: found - provisioned,
      provision_to_start_seconds: provisioned - started,
      ...(completed ? { github_completed_at: at(completed), execution_seconds: started - completed } : {}),
    },
    resources: resources[id] ?? null,
  };
}

// GET /api/usage: jobs generated deterministically per UTC day, so a range always shows the same numbers and every
// grouping adds up to the same totals.
const repositories = [
  { name: "garden-lab/papertrail", pool: "default", hosts: ["local"], perDay: 14 },
  { name: "garden-lab/pattern-kit", pool: "build", hosts: ["builder-west"], perDay: 9 },
  { name: "garden-lab/forecast", pool: "default", hosts: ["local", "builder-west"], perDay: 6 },
  { name: "garden-lab/first-workflow", pool: "default", hosts: ["local"], perDay: 2 },
];

function random(seed: number) {
  let x = seed | 0;
  return () => {
    x = (x + 0x6d2b79f5) | 0;
    let v = Math.imul(x ^ (x >>> 15), 1 | x);
    v = (v + Math.imul(v ^ (v >>> 7), 61 | v)) ^ v;
    return ((v ^ (v >>> 14)) >>> 0) / 4294967296;
  };
}

interface UsageJob { day: string; repository: string; pool: string; host: string; failed: boolean; runner: number; wait: number }

function jobsOn(day: string): UsageJob[] {
  const index = Math.floor(Date.parse(day + "T00:00:00Z") / 86400000);
  const weekend = [0, 6].includes(new Date(index * 86400000).getUTCDay());
  return repositories.flatMap((repository, r) => {
    const next = random(index * 31 + r);
    const count = Math.round(repository.perDay * (weekend ? 0.3 : 1) * (0.6 + next() * 0.8));
    return Array.from({ length: count }, () => {
      const failed = next() < 0.06;
      const neverStarted = failed && next() < 0.5;
      return {
        day, repository: repository.name, pool: repository.pool, failed,
        host: neverStarted ? "" : repository.hosts[Math.floor(next() * repository.hosts.length)],
        runner: neverStarted ? 0 : Math.round(60 + next() * 900),
        wait: neverStarted ? 300 + Math.round(next() * 3000) : Math.round(2 + next() ** 3 * 240),
      };
    });
  });
}

const percentile = (sorted: number[], p: number) => (sorted.length ? sorted[Math.min(sorted.length, Math.max(1, Math.ceil(p * sorted.length))) - 1] : 0);

function totals(jobs: UsageJob[]) {
  const waits = jobs.map((job) => job.wait).sort((a, b) => a - b);
  return {
    jobs: jobs.length,
    failed: jobs.filter((job) => job.failed).length,
    runner_seconds: jobs.reduce((sum, job) => sum + job.runner, 0),
    wait_count: waits.length,
    wait_avg_seconds: waits.length ? Math.floor(waits.reduce((a, b) => a + b, 0) / waits.length) : 0,
    wait_p50_seconds: percentile(waits, 0.5),
    wait_p95_seconds: percentile(waits, 0.95),
  };
}

function usage(query: URLSearchParams): UsageReport {
  const today = new Date().toISOString().slice(0, 10);
  const to = query.get("to") || today;
  const from = query.get("from") || new Date(Date.parse(to + "T00:00:00Z") - 29 * 86400000).toISOString().slice(0, 10);
  const group = (query.get("group") || "day") as "day" | "repository" | "pool" | "host";
  const repository = (query.get("repository") || "").trim().toLowerCase();
  const days: string[] = [];
  for (let t = Date.parse(from + "T00:00:00Z"); t <= Date.parse(to + "T00:00:00Z") && days.length < 400; t += 86400000) {
    days.push(new Date(t).toISOString().slice(0, 10));
  }
  const jobs = days.filter((day) => day <= today).flatMap(jobsOn).filter((job) => !repository || job.repository === repository);
  const groups = new Map<string, UsageJob[]>(group === "day" ? days.map((day) => [day, []]) : []);
  for (const job of jobs) groups.set(job[group], [...(groups.get(job[group]) ?? []), job]);
  const series = [...groups].map(([key, list]) => ({ key, ...totals(list) }));
  if (group === "day") series.sort((a, b) => a.key.localeCompare(b.key));
  else series.sort((a, b) => b.jobs - a.jobs || a.key.localeCompare(b.key));
  return { from, to, group, totals: totals(jobs), series };
}

// GET /api/usage/rightsizing (job_resources.go readRightsizing).
function rightsizing(query: URLSearchParams): RightsizingReport {
  const group = query.get("group") === "job" ? "job" : "pool";
  const days = Math.min(7, Math.max(1, Number(query.get("days")) || 7));
  const scale = (n: number) => Math.max(1, Math.round((n * days) / 7));
  const resource = (limit: number, peak: number, ratio: number | null, verdict: string) =>
    ({ limit, peak_max: peak, ...(ratio === null ? {} : { peak_ratio: ratio }), verdict });
  const groups = group === "pool" ? [
    { key: "default", jobs: scale(148), sampled_jobs: scale(131), oom_kills: 2, cpu: resource(2, 1.97, 0.99, "tight"), memory: resource(4096, 4096, 1, "tight") },
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

// GET /api/elastic.
function elastic(): ElasticReport {
  const decision = (seconds: number, runner: string, resource: string, from: number, to: number, reason: string, extra = {}) =>
    ({ at: at(seconds), host: "local", runner, resource, from, to, reason, applied: true, ...extra });
  return {
    mode: "apply",
    interval_seconds: 10,
    hosts: [
      { host: "builder-west", cpus: 8, memory_mib: 16384, held_slots: 0, surplus_cpus: 3, lent_cpus: 0, surplus_memory_mib: 7680, extra_memory_mib: 0 },
      { host: "local", cpus: 8, memory_mib: 16384, held_slots: 1, surplus_cpus: 1, lent_cpus: 1, surplus_memory_mib: 3584, extra_memory_mib: 1024 },
    ],
    decisions: [
      decision(40, "runquill-demo-4826", "cpu", 2, 3, "busy at 1.92 of 2.00 CPUs; borrowing 1.00 unused host CPUs"),
      decision(310, "runquill-demo-4826", "memory", 4096, 5120, "memory at 93% of 4096 MiB; adding 1024 MiB no other runner was promised"),
      decision(900, "runquill-demo-4817", "cpu", 3, 2, "idle at 0.40 of 2.00 guaranteed CPUs; returning 1.00 borrowed CPUs"),
      decision(1500, "runquill-demo-4810", "cpu", 4, 3, "host surplus is 1.00 CPUs but 2.00 are lent; returning 1.00 borrowed CPUs", { forced: true }),
    ],
  };
}

// GET /api/hosts/resources. Disk use appears once the viewer asks for it (?disk=1), as on a live panel.
let diskMeasuredAt: string | undefined;
function hostResources(query: URLSearchParams): ResourceReport {
  if (query.get("disk") === "1") diskMeasuredAt ??= new Date().toISOString();
  const gib = 1024 ** 3;
  const disk = (images: number, imageBytes: number, volumes: number, cacheBytes: number) =>
    diskMeasuredAt ? { disk: { images, image_bytes: imageBytes, cache_volumes: volumes, cache_bytes: cacheBytes } } : {};
  const reserved = (runners: number, cpus: number, memory: number) =>
    ({ runners, runner_cpus: cpus, runner_memory_bytes: memory, cpus: runners * cpus, memory_bytes: runners * memory });
  return {
    disk_measuring: false,
    disk_measured_at: diskMeasuredAt,
    hosts: [
      { host: "local", cpus: 8, memory_bytes: 16 * gib, reserved: { ...reserved(1, 3, 5 * gib), runner_cpus: 2, runner_memory_bytes: 4 * gib },
        usage: { cpus: 1.92, memory_bytes: 4.6 * gib }, ...disk(2, 3.1 * gib, 4, 2.4 * gib) },
      { host: "builder-west", cpus: 8, memory_bytes: 16 * gib, reserved: reserved(1, 2, 4 * gib), ...disk(1, 1.6 * gib, 2, 0.8 * gib) },
      { host: "builder-east", cpus: 16, memory_bytes: 32 * gib, reserved: reserved(0, 2, 4 * gib), ...disk(0, 0, 0, 0) },
      { host: "agent://edge-1", unavailable: "agent", reserved: reserved(0, 0, 0) },
    ],
  };
}

const routes: Record<string, (query: URLSearchParams) => unknown> = {
  "GET /api/jobs": history,
  "GET /api/usage": usage,
  "GET /api/usage/rightsizing": rightsizing,
  "GET /api/elastic": elastic,
  "GET /api/hosts/resources": hostResources,
};

/** A GET's sample answer, or undefined (the request is refused like any other in the sample panel). */
export function answer(path: string, query: URLSearchParams): unknown {
  const job = /^\/api\/jobs\/(\d+)$/.exec(path);
  if (job) return detail(Number(job[1])) ?? null;
  return routes["GET " + path]?.(query);
}
