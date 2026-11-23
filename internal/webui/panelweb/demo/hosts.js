// Sample responses for the Hosts & pools view: the same shapes as GET /api/elastic and GET /api/hosts/resources.
function elastic() {
  const at = (seconds) => new Date(Date.now() - seconds * 1000).toISOString();
  const decision = (seconds, runner, resource, from, to, reason, applied, extra = {}) =>
    ({ at: at(seconds), host: "local", runner, resource, from, to, reason, applied, ...extra });
  return {
    mode: "apply",
    interval_seconds: 10,
    hosts: [
      { host: "builder-west", cpus: 8, memory_mib: 16384, held_slots: 0, surplus_cpus: 3, lent_cpus: 0, surplus_memory_mib: 7680, extra_memory_mib: 0 },
      { host: "local", cpus: 8, memory_mib: 16384, held_slots: 1, surplus_cpus: 1, lent_cpus: 1, surplus_memory_mib: 3584, extra_memory_mib: 1024 },
    ],
    runners: [],
    decisions: [
      decision(40, "runquill-demo-4826", "cpu", 2, 3, "busy at 1.92 of 2.00 CPUs; borrowing 1.00 unused host CPUs", true),
      decision(310, "runquill-demo-4826", "memory", 4096, 5120, "memory at 93% of 4096 MiB; adding 1024 MiB no other runner was promised", true),
      decision(900, "runquill-demo-4817", "cpu", 3, 2, "idle at 0.40 of 2.00 guaranteed CPUs; returning 1.00 borrowed CPUs", true),
      decision(1500, "runquill-demo-4810", "cpu", 4, 3, "host surplus is 1.00 CPUs but 2.00 are lent; returning 1.00 borrowed CPUs", true, { forced: true }),
    ],
  };
}

// Disk use appears once the viewer asks for it (?disk=1), as on a live panel.
let diskMeasuredAt = null;
function resources(params) {
  if (params.get("disk") === "1") diskMeasuredAt ??= new Date().toISOString();
  const gib = 1024 ** 3;
  const disk = (images, imageBytes, volumes, cacheBytes) => diskMeasuredAt
    ? { disk: { images, image_bytes: imageBytes, cache_volumes: volumes, cache_bytes: cacheBytes } } : {};
  const reserved = (runners, cpus, memory) => ({ runners, runner_cpus: cpus, runner_memory_bytes: memory,
    cpus: runners * cpus, memory_bytes: runners * memory });
  return {
    disk_measuring: false,
    ...(diskMeasuredAt ? { disk_measured_at: diskMeasuredAt } : {}),
    hosts: [
      { host: "local", cpus: 8, memory_bytes: 16 * gib, reserved: { ...reserved(1, 3, 5 * gib), runner_cpus: 2, runner_memory_bytes: 4 * gib },
        usage: { cpus: 1.92, memory_bytes: 4.6 * gib }, ...disk(2, 3.1 * gib, 4, 2.4 * gib) },
      { host: "builder-west", cpus: 8, memory_bytes: 16 * gib, reserved: reserved(1, 2, 4 * gib), ...disk(1, 1.6 * gib, 2, 0.8 * gib) },
      { host: "builder-east", cpus: 16, memory_bytes: 32 * gib, reserved: reserved(0, 2, 4 * gib), ...disk(0, 0, 0, 0) },
      { host: "agent://edge-1", unavailable: "agent", reserved: reserved(0, 0, 0) },
    ],
  };
}

export default { "GET /api/elastic": elastic, "GET /api/hosts/resources": resources };
