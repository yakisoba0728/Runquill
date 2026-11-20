// Sample responses for the Hosts & pools view: the same shape as GET /api/elastic.
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

export default { "GET /api/elastic": elastic };
