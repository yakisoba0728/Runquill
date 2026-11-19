package main

type jobDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *statusReport) explainJob(j statusJob, jobPhase phase, hosts []hostCapacity) *jobDiagnostic {
	if j.PoolError != "" && (j.Status == "WAITING" || j.Status == "RETRY") && j.Runner == "" {
		return &jobDiagnostic{"pool_rejected", j.PoolError}
	}
	if j.Runner != "" {
		for _, host := range hosts {
			if j.RunnerHost != "" && j.RunnerHost != host.Name {
				continue
			}
			if detail, ok := host.RunnerDiagnostics[j.Runner]; ok {
				return &detail
			}
		}
	}
	switch j.Status {
	case "RETRY":
		return &jobDiagnostic{"retry_scheduled", "Runquill scheduled another attempt."}
	case "WAITING":
		return r.explainWaiting(j.Pool, hosts)
	case "STARTING":
		return &jobDiagnostic{"runner_starting", "Waiting for the assigned runner to start."}
	case "OFFLINE":
		return &jobDiagnostic{"runner_host_offline", "The assigned worker is offline. Its slot stays reserved until Runquill can reach it."}
	case "UNKNOWN":
		return &jobDiagnostic{"runner_status_unknown", "The assigned runner could not be observed. Check the worker connection."}
	case "PAUSED":
		return &jobDiagnostic{"runner_paused", "The runner container is paused on its worker."}
	case "RUNNING":
		if jobPhase == queued {
			return &jobDiagnostic{"waiting_for_github", "The runner is running. Runquill has yet to observe GitHub start the job."}
		}
	case "CLEANUP":
		return &jobDiagnostic{"runner_cleanup", "The runner lease is awaiting cleanup."}
	}
	return nil
}

func (r *statusReport) explainWaiting(pool string, hosts []hostCapacity) *jobDiagnostic {
	if r.Runners == nil {
		return &jobDiagnostic{"fleet_status_unknown", "Waiting for a worker status snapshot."}
	}

	var matching, paused, offline, unavailable int
	for _, host := range hosts {
		if pool != "" && pool != host.Pool {
			continue
		}
		matching++
		switch {
		case host.Cordoned:
			paused++
		case host.Containers == nil:
			offline++
		case host.Err != nil || host.ImageWait != "":
			unavailable++
		}
	}
	switch {
	case matching == 0:
		return &jobDiagnostic{"pool_no_workers", "No worker belongs to this job's pool. Check the pool configuration."}
	case paused == matching:
		return &jobDiagnostic{"workers_cordoned", "Matching workers are paused for maintenance. Resume a worker to assign this job."}
	case paused+offline == matching:
		return &jobDiagnostic{"workers_offline", "The matching workers accepting jobs are offline. Check their connections."}
	case paused+offline+unavailable == matching:
		return &jobDiagnostic{"workers_unavailable", "The matching workers accepting jobs are unavailable. Check worker health and runner images."}
	case r.Runners.OccupiedSlots+r.Runners.ReservedSlots >= r.SlotLimit:
		return &jobDiagnostic{"fleet_full", "The fleet has reached its runner slot limit. Running and reserved slots both count."}
	case availableHost(hosts, pool) == nil:
		return &jobDiagnostic{"workers_full", "Reachable workers eligible for this job have no free slots."}
	default:
		return &jobDiagnostic{"waiting_scheduler", "Waiting for Runquill to assign an available runner."}
	}
}
