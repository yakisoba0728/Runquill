// Sample responses for the Jobs view: the same shapes as GET /api/jobs and
// GET /api/jobs/{id}. The Queue view's fixtures reuse detail().
const at = (seconds) => new Date(Date.now() - seconds * 1000).toISOString();

function rows() {
  const row = (id, repository, outcome, extra = {}) => ({
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

function history(query) {
  const outcome = query.get("outcome");
  const repository = query.get("repository");
  const since = Number(query.get("since")) * 1000;
  return {
    jobs: rows().filter((job) => (!outcome || job.outcome === outcome) &&
      (!repository || job.repository === repository.toLowerCase()) &&
      (!since || Date.parse(job.queued_at) >= since)),
  };
}

// Stage timing, as seconds before now for each recorded moment, and sampled
// resources per job. Jobs that never got a runner have neither.
const timings = {
  4817: [510, 506, 500, 494, 479],
  4823: [2302, 2300, 2296, 2290, 1720],
  4824: [221, 220, 217, 213],
  4825: [900, 899, 896, 890, 24],
  4826: [120, 119, 114, 110],
};

const resources = {
  4817: { samples: 3, cpu_limit: 2, memory_limit_mib: 4096, cpu_peak: 0.41, cpu_average: 0.22, memory_peak_mib: 612, oom_killed: false },
  4823: { samples: 57, cpu_limit: 2, memory_limit_mib: 4096, cpu_limit_max: 2, memory_limit_max_mib: 4096,
    cpu_peak: 1.97, cpu_average: 1.41, memory_peak_mib: 4096, oom_killed: true },
  4825: { samples: 87, cpu_limit: 2, memory_limit_mib: 4096, cpu_peak: 1.12, cpu_average: 0.74, memory_peak_mib: 4096, oom_killed: true },
  4826: { samples: 11, cpu_limit: 2, memory_limit_mib: 4096, cpu_limit_max: 3, memory_limit_max_mib: 5120,
    cpu_peak: 2.71, cpu_average: 1.62, memory_peak_mib: 4710 },
};

function timing(id) {
  const moments = timings[id];
  if (!moments) return undefined;
  const [created, found, provisioned, started, completed] = moments;
  return {
    discovery_source: "webhook", github_created_at: at(created), discovered_at: at(found), provisioned_at: at(provisioned),
    github_started_at: at(started), creation_to_discovery_seconds: created - found, runquill_queue_seconds: found - provisioned,
    provision_to_start_seconds: provisioned - started,
    ...(completed ? { github_completed_at: at(completed), execution_seconds: started - completed } : {}),
  };
}

// detail answers GET /api/jobs/{id}; a job outside the sample history gets a bare row.
export function detail(id) {
  const job = rows().find((candidate) => candidate.id === id) || { id, repository: "garden-lab/sample", outcome: "running" };
  return { ...job, timing: timing(id), resources: resources[id] };
}

export const detailRoutes = (ids) => Object.fromEntries(ids.map((id) => ["GET /api/jobs/" + id, () => detail(id)]));

export default { "GET /api/jobs": history, ...detailRoutes([4817, 4819, 4821, 4823, 4824, 4826, 4828]) };
