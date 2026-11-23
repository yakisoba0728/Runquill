package app

import (
	"cmp"
	"strings"
)

type jobDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type jobPlacementDiagnostics struct {
	images  *runnerImages
	runtime runnerRuntime
	repoCap int
}

func (r *statusReport) configure(backend, repositoryScope string, pools workerPools, jobs []job, hosts []hostCapacity, placement jobPlacementDiagnostics) {
	r.pools, r.Backend, r.RepositoryScope = pools, backend, repositoryScope
	for i := range r.Hosts {
		// Outbound workers never keep warm runners (worker_pools.go refuses the setting there).
		if host := &r.Hosts[i]; !strings.HasPrefix(host.Name, "agent://") {
			host.WarmTarget = pools[cmp.Or(host.Pool, defaultWorkerPool)].Warm
		}
	}
	active := make(map[[2]string]int)
	for i, j := range jobs {
		pool, err := pools.forJob(j)
		r.Jobs[i].Pool = pool
		if err != nil {
			r.Jobs[i].PoolError = err.Error()
		}
		if j.runnerName != "" {
			active[[2]string{pool, j.repository}]++
		}
	}
	r.describeImages(placement.images, jobs)
	for i, j := range jobs {
		row := &r.Jobs[i]
		policy := pools[row.Pool]
		limit := repositoryRunnerCap(policy, placement.repoCap)
		capped := limit > 0 && active[[2]string{row.Pool, j.repository}] >= limit
		selected, err := placement.images.forJob(j.route, policy)
		var canRun func(hostCapacity) (bool, bool)
		if selected.Image != "" {
			if targets, ok := unwrapRuntime(placement.runtime).(runnerImageRuntime); ok {
				canRun = func(host hostCapacity) (bool, bool) { return targets.canRun(host.Name, selected.Image) }
			}
		}
		row.Diagnostic = r.explainJob(*row, j.phase, hosts, capped, err, canRun)
	}
}

func (r *statusReport) explainJob(j statusJob, jobPhase phase, hosts []hostCapacity, capped bool, imageErr error, canRun func(hostCapacity) (bool, bool)) *jobDiagnostic {
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
		return retryDiagnostic(j.RetryReason)
	case "WAITING":
		if imageErr != nil {
			return &jobDiagnostic{"runner_image_rejected", imageErr.Error()}
		}
		if capped {
			return &jobDiagnostic{"repository_cap", "This repository has reached its runner limit in the worker pool."}
		}
		return r.explainWaiting(j, hosts, canRun)
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

func retryDiagnostic(reason string) *jobDiagnostic {
	switch {
	case strings.HasPrefix(reason, imageWaitPrefix):
		return &jobDiagnostic{"runner_image_pending", reason}
	case strings.HasPrefix(reason, "select runner image: "):
		return &jobDiagnostic{"runner_image_rejected", reason}
	case strings.HasPrefix(reason, "provision runner: waiting for GitHub to queue job"):
		return &jobDiagnostic{"waiting_for_github_queue", reason}
	default:
		if reason == "" {
			reason = "Runquill scheduled another attempt."
		}
		return &jobDiagnostic{"retry_scheduled", reason}
	}
}

func (r *statusReport) explainWaiting(j statusJob, hosts []hostCapacity, canRun func(hostCapacity) (bool, bool)) *jobDiagnostic {
	if r.Runners == nil {
		return &jobDiagnostic{"fleet_status_unknown", "Waiting for a worker status snapshot."}
	}

	var matching, eligible, paused, unregistered, offline, unavailable, imagePending, ready int
	for _, host := range hosts {
		if j.Pool != "" && j.Pool != host.Pool {
			continue
		}
		matching++
		own := true
		if canRun != nil {
			var matches bool
			matches, own = canRun(host)
			if !matches {
				continue
			}
		}
		eligible++
		switch {
		case host.Cordoned:
			paused++
		case host.UnregisteredWorker:
			unregistered++
		case host.Containers == nil:
			offline++
		case host.Err != nil:
			unavailable++
		case own && host.ImageWait != "":
			if host.Occupied < host.Limit {
				imagePending++
			}
		case host.Occupied < host.Limit:
			ready++
		}
	}
	switch {
	case matching == 0:
		return &jobDiagnostic{"pool_no_workers", "No worker belongs to this job's pool. Check the pool configuration."}
	case eligible == 0:
		return &jobDiagnostic{"workers_image_incompatible", "No matching worker can run the selected runner image."}
	case paused == eligible:
		return &jobDiagnostic{"workers_cordoned", "Matching workers are paused for maintenance. Resume a worker to assign this job."}
	case paused+offline == eligible:
		return &jobDiagnostic{"workers_offline", "The matching workers accepting jobs are offline. Check their connections."}
	case paused+unregistered == eligible:
		return &jobDiagnostic{"workers_unregistered", "Matching workers must enroll before they can accept jobs."}
	case paused+offline+unregistered+unavailable == eligible:
		return &jobDiagnostic{"workers_unavailable", "The matching workers accepting jobs are unavailable. Check worker health and runner images."}
	case r.Runners.OccupiedSlots+r.Runners.ReservedSlots >= r.SlotLimit:
		return &jobDiagnostic{"fleet_full", "The fleet has reached its runner slot limit. Running and reserved slots both count."}
	case strings.HasPrefix(j.RetryReason, imageWaitPrefix):
		return retryDiagnostic(j.RetryReason)
	case ready == 0 && imagePending > 0:
		return &jobDiagnostic{"runner_image_pending", "Matching workers are waiting for the runner image."}
	case ready == 0:
		return &jobDiagnostic{"workers_full", "Reachable workers eligible for this job have no free slots."}
	default:
		return &jobDiagnostic{"waiting_scheduler", "Waiting for Runquill to assign an available runner."}
	}
}
