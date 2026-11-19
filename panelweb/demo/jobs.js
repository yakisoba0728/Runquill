// Sample responses for the Jobs view: the same shape as GET /api/jobs.
function history(query) {
  const at = (seconds) => new Date(Date.now() - seconds * 1000).toISOString();
  const row = (id, repository, outcome, extra = {}) => ({
    id, repository: "garden-lab/" + repository, outcome, runner_label: "runquill-18062-1-" + repository,
    url: "https://github.com/garden-lab/" + repository + "/actions/runs/18062/job/" + id, ...extra,
  });
  const rows = [
    row(4828, "pattern-kit", "waiting", { pool: "build", runner_os: "ubuntu2004", queued_at: at(58), wait_seconds: 58,
      runner_label: "runquill-18062-1-os__ubuntu2004__check", retry_at: at(-12),
      retry_reason: 'select runner image: runner image "ubuntu2004" is not in the runner image catalogue; add it to RUNQUILL_RUNNER_IMAGES' }),
    row(4826, "papertrail", "running", { pool: "default", host: "local", queued_at: at(119), started_at: at(110), wait_seconds: 5 }),
    row(4824, "pattern-kit", "running", { pool: "build", runner_os: "ubuntu2204", host: "builder-west",
      runner_label: "runquill-18061-1-os__ubuntu2204__lint", queued_at: at(221), started_at: at(213), wait_seconds: 4 }),
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

export default { "GET /api/jobs": history };
