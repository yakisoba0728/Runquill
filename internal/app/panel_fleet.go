package app

import (
	"cmp"
	"slices"
)

type panelRunner struct {
	Name         string `json:"name"`
	Host         string `json:"host,omitempty"`
	Pool         string `json:"pool,omitempty"`
	RuntimeState string `json:"runtime_state,omitempty"`
	JobID        int64  `json:"job_id,omitempty"`
	RunnerOS     string `json:"runner_os,omitempty"`
	RunnerImage  string `json:"runner_image,omitempty"`
}

func panelRunnerInventory(hosts []hostCapacity, jobs []statusJob) []panelRunner {
	runners := make([]panelRunner, 0)
	positions := make(map[[2]string]int)
	byName := make(map[string]int)
	for _, host := range hosts {
		for name, state := range host.Containers {
			index := len(runners)
			positions[[2]string{host.Name, name}] = index
			if _, exists := byName[name]; exists {
				byName[name] = -1
			} else {
				byName[name] = index
			}
			runners = append(runners, panelRunner{Name: name, Host: host.Name, Pool: host.Pool, RuntimeState: state})
		}
	}
	for _, job := range jobs {
		if job.Runner == "" {
			continue
		}
		index, found := positions[[2]string{job.RunnerHost, job.Runner}]
		if job.RunnerHost == "" {
			index, found = byName[job.Runner]
		}
		if found && index >= 0 && runners[index].JobID == 0 {
			runners[index].JobID, runners[index].RunnerOS, runners[index].RunnerImage = job.ID, job.RunnerOS, job.RunnerImage
		} else {
			runners = append(runners, panelRunner{Name: job.Runner, Host: job.RunnerHost, Pool: job.Pool, JobID: job.ID,
				RunnerOS: job.RunnerOS, RunnerImage: job.RunnerImage})
		}
	}
	slices.SortFunc(runners, func(a, b panelRunner) int {
		return cmp.Or(cmp.Compare(a.Host, b.Host), cmp.Compare(a.Name, b.Name), cmp.Compare(a.JobID, b.JobID))
	})
	return runners
}
