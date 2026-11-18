const notes = {
  first: "The first job finished on your server and its runner was removed.",
  missed: "GitHub's queued webhook was missed. Polling discovered the job and started its runner.",
  offline: "The disconnected worker keeps its leased slot. A healthy host can still run the next job.",
};

export function sample(name) {
  const at = (seconds) => new Date(Date.now() - seconds * 1000).toISOString();
  const host = (name, limit, occupied, reserved = 0) => ({
    name, status: reserved ? "OFFLINE" : "UP", slot_limit: limit,
    occupied_slots: occupied, reserved_slots: reserved,
  });
  const job = (id, repository, status, runner_host, extra = {}) => ({
    id, repository: "garden-lab/" + repository, status, runner_host, ...extra,
  });
  const timing = (source, created, found, provisioned, started) => ({
    discovery_source: source, github_created_at: at(created), discovered_at: at(found),
    provisioned_at: provisioned === undefined ? null : at(provisioned),
    github_started_at: started === undefined ? null : at(started),
  });

  let hosts = [host("local", 2, 1), host("builder-west", 2, 1)];
  let jobs = [
    job(4826, "papertrail", "RUNNING", "local", { timing: timing("webhook", 120, 119, 114, 110) }),
    job(4824, "pattern-kit", "RUNNING", "builder-west", { timing: timing("webhook", 221, 220, 217, 213) }),
    job(4827, "forecast", "WAITING", "", { queue_position: 1, timing: timing("webhook", 10, 9) }),
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
      job(4835, "pattern-kit", "OFFLINE", "builder-west", { timing: timing("webhook", 260, 259, 255, 250) }),
      job(4836, "forecast", "RUNNING", "local", { timing: timing("webhook", 81, 80, 75, 71) }),
      job(4838, "papertrail", "RUNNING", "local", { timing: timing("poll", 62, 44, 39, 33) }),
      job(4840, "forecast", "WAITING", "", { queue_position: 1, timing: timing("webhook", 9, 8) }),
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
      fleet: {
        slot_limit: 4, hosts, jobs,
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
