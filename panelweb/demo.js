const notes = {
  first: "The first job finished on your server and its runner was removed.",
  missed: "GitHub's queued webhook was missed. Polling discovered the job and started its runner.",
  offline: "The disconnected worker keeps its leased slot. A healthy host can still run the next job.",
};

export function sample(name) {
  const at = (seconds) => new Date(Date.now() - seconds * 1000).toISOString();
  const host = (name, limit, occupied, reserved = 0) => ({
    name, pool: name === "local" ? "default" : "build", status: reserved ? "OFFLINE" : "UP", slot_limit: limit,
    occupied_slots: occupied, reserved_slots: reserved,
  });
  const job = (id, repository, status, runner_host, extra = {}) => ({
    id, repository: "garden-lab/" + repository, status, runner_host,
    pool: runner_host === "builder-west" ? "build" : "default",
    runner: runner_host ? "runquill-demo-" + id : "",
    url: "https://github.com/garden-lab/" + repository + "/actions/runs/18062/job/" + id,
    ...extra,
  });
  const timing = (source, created, found, provisioned, started) => ({
    discovery_source: source, github_created_at: at(created), discovered_at: at(found),
    provisioned_at: provisioned === undefined ? null : at(provisioned),
    github_started_at: started === undefined ? null : at(started),
    creation_to_discovery_seconds: created - found,
    runquill_queue_seconds: found - (provisioned ?? 0),
    provision_to_start_seconds: started === undefined ? null : provisioned - started,
  });

  let hosts = [host("local", 2, 1), host("builder-west", 2, 1)];
  let jobs = [
    job(4826, "papertrail", "RUNNING", "local", { timing: timing("webhook", 120, 119, 114, 110) }),
    job(4824, "pattern-kit", "RUNNING", "builder-west", { timing: timing("webhook", 221, 220, 217, 213) }),
    job(4827, "forecast", "WAITING", "", { queue_position: 1, runner_label: "runquill-18062-1-forecast",
      diagnostic: { code: "waiting_scheduler", message: "Waiting for Runquill to assign an available runner." },
      timing: timing("webhook", 10, 9) }),
    job(4828, "pattern-kit", "RETRY", "", { pool: "build", retry_at: at(-12), retry_reason: "Waiting to retry runner provisioning.",
      timing: timing("poll", 73, 58) }),
  ];
  let match = at(8);
  let firstSeen = true;
  const cleaned = {
    id: 4817, repository: "garden-lab/first-workflow", runner_host: "local",
    timing: timing("webhook", 510, 506, 500, 494), finished_at: at(479), cleaned_at: at(475),
  };
  let firstClean = cleaned;
  if (name === "missed") {
    hosts = [host("local", 2, 1), host("builder-west", 2, 0)];
    jobs = [job(4831, "papertrail", "RUNNING", "local", { timing: timing("poll", 61, 31, 25, 18) })];
    match = at(490);
    firstSeen = false;
    firstClean = null;
  }
  if (name === "offline") {
    hosts = [host("local", 2, 2), host("builder-west", 2, 0, 1)];
    jobs = [
      job(4835, "pattern-kit", "OFFLINE", "builder-west", { runner_label: "runquill-18048-1-pattern",
        diagnostic: { code: "runner_host_offline", message: "The assigned worker is offline. Its slot stays reserved until Runquill can reach it." },
        timing: timing("webhook", 260, 259, 255, 250) }),
      job(4836, "forecast", "RUNNING", "local", { timing: timing("webhook", 81, 80, 75, 71) }),
      job(4838, "papertrail", "RUNNING", "local", { timing: timing("poll", 62, 44, 39, 33) }),
      job(4840, "forecast", "WAITING", "", { queue_position: 1, runner_label: "runquill-18063-1-forecast",
        diagnostic: { code: "workers_full", message: "Reachable workers eligible for this job have no free slots." },
        timing: timing("webhook", 9, 8) }),
    ];
    firstSeen = true;
    firstClean = cleaned;
  }
  return {
    note: notes[name],
    snapshot: {
      webhook: true, polling: true, last_webhook: match, last_poll: at(3), sampled_at: at(0),
      first_job_seen: firstSeen, first_job_clean: firstClean,
      recent: firstClean ? [cleaned] : [],
      runner_inventory: jobs.filter((job) => job.runner).map((job) => ({
        name: job.runner, job_id: job.id, host: job.runner_host, pool: job.pool,
        runtime_state: job.status === "RUNNING" ? "running" : undefined,
      })),
      fleet: {
        slot_limit: 4, repository_scope: "selected", hosts, jobs,
        runners: {
          known_running: hosts.reduce((n, h) => n + h.occupied_slots, 0),
          occupied_slots: hosts.reduce((n, h) => n + h.occupied_slots, 0),
          reserved_slots: hosts.reduce((n, h) => n + h.reserved_slots, 0),
          offline_hosts: hosts.filter((h) => h.status === "OFFLINE").length,
        },
      },
    },
  };
}

// Sample job history with the same shape as GET /api/jobs.
export function history(query) {
  const at = (seconds) => new Date(Date.now() - seconds * 1000).toISOString();
  const row = (id, repository, outcome, extra = {}) => ({
    id, repository: "garden-lab/" + repository, outcome, runner_label: "runquill-18062-1-" + repository,
    url: "https://github.com/garden-lab/" + repository + "/actions/runs/18062/job/" + id, ...extra,
  });
  const rows = [
    row(4828, "pattern-kit", "waiting", { pool: "build", queued_at: at(58), wait_seconds: 58,
      retry_at: at(-12), retry_reason: "Waiting to retry runner provisioning." }),
    row(4826, "papertrail", "running", { pool: "default", host: "local", queued_at: at(119), started_at: at(110), wait_seconds: 5 }),
    row(4821, "forecast", "failed", { pool: "default", queued_at: at(90000), finished_at: at(3600), wait_seconds: 86400,
      failure: "not started within 24 hours" }),
    row(4819, "pattern-kit", "failed", { pool: "build", queued_at: at(5400), finished_at: at(5100), wait_seconds: 300,
      failure: "cancelled by @garden-admin" }),
    row(4817, "first-workflow", "finished", { pool: "default", host: "local", queued_at: at(506), started_at: at(494),
      finished_at: at(479), cleaned_at: at(475), wait_seconds: 6 }),
  ];
  const outcome = query.get("outcome");
  const repository = query.get("repository");
  const since = Number(query.get("since")) * 1000;
  return {
    jobs: rows.filter((job) => (!outcome || job.outcome === outcome) &&
      (!repository || job.repository === repository.toLowerCase()) &&
      (!since || Date.parse(job.queued_at) >= since)),
  };
}
