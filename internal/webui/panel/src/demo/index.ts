// Sample fleet snapshots for the sample panel (`runquill panel --demo` and GitHub Pages), ported from panelweb/demo.js.
// Everything under src/demo/ builds into assets/demo-*.js only, which live panels refuse to serve. Import nothing from
// the rest of src except types, or shared code would be pulled into the demo chunk.
import type { Job, Snapshot } from "../types";

export { answer } from "./fixtures";

export const scenarios = ["first", "missed", "offline"] as const;
export type Scenario = (typeof scenarios)[number];

// The screen (and job) each scenario is about; switching scenario opens it.
const focus: Record<Scenario, string> = { first: "#/overview", missed: "#/jobs/4831", offline: "#/jobs/4835" };

// English i18n keys.
const notes: Record<Scenario, string> = {
  first: "The first job finished on your server and its runner was removed.",
  missed: "GitHub's queued webhook was missed. Polling discovered the job and started its runner.",
  offline: "The disconnected worker keeps its leased slot. A healthy host can still run the next job.",
};

type SampleJob = Job & Record<string, unknown>;

export function sample(name: Scenario) {
  const at = (seconds: number) => new Date(Date.now() - seconds * 1000).toISOString();
  const host = (name: string, limit: number, occupied: number, reserved = 0, extra = {}) => ({
    name, pool: name === "local" ? "default" : "build", status: reserved ? "OFFLINE" : "UP", slot_limit: limit,
    occupied_slots: occupied, reserved_slots: reserved, image_status: reserved ? undefined : "ready", last_seen: reserved ? at(1260) : undefined, ...extra,
  });
  const runnerImage = "ghcr.io/yakisoba0728/runquill-runner:v0.3.3";
  const job = (id: number, repository: string, status: string, runner_host: string, extra = {}): SampleJob => ({
    id, repository: "garden-lab/" + repository, status, runner_host,
    pool: runner_host === "builder-west" ? "build" : "default",
    runner: runner_host ? "runquill-demo-" + id : "",
    url: "https://github.com/garden-lab/" + repository + "/actions/runs/18062/job/" + id,
    ...extra,
  });
  const timing = (source: string, created: number, found: number, provisioned?: number, started?: number) => ({
    discovery_source: source, github_created_at: at(created), discovered_at: at(found),
    provisioned_at: provisioned === undefined ? null : at(provisioned),
    github_started_at: started === undefined ? null : at(started),
    creation_to_discovery_seconds: created - found,
    runquill_queue_seconds: found - (provisioned ?? 0),
    provision_to_start_seconds: started === undefined || provisioned === undefined ? null : provisioned - started,
  });

  // builder-east is still pulling its pool's runner image. The default pool keeps one warm runner per host, the build pool two.
  let hosts = [host("local", 2, 1, 0, { warm: 1, warm_target: 1 }), host("builder-west", 2, 1, 0, { warm: 1, warm_target: 2 }),
    host("builder-east", 2, 0, 0, { status: "UNAVAILABLE", image_status: "pulling", warm_target: 2,
      error: "pulling runner image ghcr.io/garden-lab/runner-build:2026.11" }),
    host("agent://edge-1", 1, 0, 0, { image_status: undefined })];
  let jobs = [
    job(4826, "papertrail", "RUNNING", "local", { runner_image: runnerImage, timing: timing("webhook", 120, 119, 114, 110) }),
    job(4824, "pattern-kit", "RUNNING", "builder-west", { runner_label: "runquill-18061-1-os__ubuntu2204__lint",
      runner_os: "ubuntu2204", runner_image: runnerImage + "-ubuntu22.04", timing: timing("webhook", 221, 220, 217, 213) }),
    job(4825, "forecast", "CLEANUP", "local", { runner_label: "runquill-18060-1-forecast",
      diagnostic: { code: "runner_memory_limit", message: "The container engine reported an out-of-memory kill. Check the worker pool memory limit." },
      timing: timing("webhook", 900, 899, 896, 890) }),
    job(4827, "forecast", "WAITING", "", { queue_position: 1, runner_label: "runquill-18062-1-os__debian13__forecast",
      runner_os: "debian13", runner_image: runnerImage + "-debian13",
      retry_reason: "waiting for runner image " + runnerImage + "-debian13 on local",
      diagnostic: { code: "waiting_scheduler", message: "Waiting for Runquill to assign an available runner." },
      timing: timing("webhook", 10, 9) }),
    job(4828, "pattern-kit", "RETRY", "", { pool: "build", retry_at: at(-12), runner_label: "runquill-18062-1-os__ubuntu2004__check",
      runner_os: "ubuntu2004",
      retry_reason: 'select runner image: runner image "ubuntu2004" is not in the runner image catalogue; add it to RUNQUILL_RUNNER_IMAGES',
      timing: timing("poll", 73, 58) }),
  ];
  let match: string | undefined = at(8);
  let firstSeen = true;
  const cleaned = {
    id: 4817, repository: "garden-lab/first-workflow", runner_host: "local",
    timing: timing("webhook", 510, 506, 500, 494), finished_at: at(479), cleaned_at: at(475),
  };
  let firstClean: typeof cleaned | null = cleaned;
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
  const snapshot: Snapshot = {
    webhook: true, polling: true, last_webhook: match, last_poll: at(3), sampled_at: at(0),
    first_job_seen: firstSeen, first_job_clean: firstClean,
    recent: firstClean ? [{ ...cleaned, pool: "default", url: "https://github.com/garden-lab/first-workflow/actions/runs/17990/job/4817" }] : [],
    runner_inventory: jobs.filter((job) => job.runner).map((job) => ({
      name: job.runner!, job_id: job.id, host: job.runner_host, pool: job.pool,
      runtime_state: ({ RUNNING: "running", CLEANUP: "exited" } as Record<string, string>)[job.status!],
      runner_os: job.runner_os, runner_image: job.runner_image,
    })),
    fleet: {
      slot_limit: 4, repository_scope: "selected", hosts, jobs,
      runners: {
        known_running: hosts.reduce((n, h) => n + h.occupied_slots, 0),
        occupied_slots: hosts.reduce((n, h) => n + h.occupied_slots, 0),
        reserved_slots: hosts.reduce((n, h) => n + h.reserved_slots, 0),
      },
    },
  };
  return { note: notes[name], focus: focus[name], snapshot };
}

export type Sample = typeof sample;
