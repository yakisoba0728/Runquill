package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	retryDelay      = 15 * time.Second
	maxLaunchDelay  = 10 * time.Minute
	idleTimeout     = 5 * time.Minute
	completionGrace = 30 * time.Second
	queueExpiry     = 24 * time.Hour
)

type runnerRuntime interface {
	snapshot(context.Context) ([]dockerHostSnapshot, error)
	startOn(context.Context, string, string, string) error
	removeOn(context.Context, string, string) error
}

type scheduler struct {
	queue      *queue
	github     *github
	runtime    runnerRuntime
	limit      int
	hostLimits map[string]int
	panel      *panelState
	pools      workerPools
	metrics    *fleetMetrics
	operations *operations
	alerts     *fleetAlerts
	// maxPerRepository caps runners per repository in pools without their own cap; 0 is unlimited.
	maxPerRepository int
	// images is the runner image catalogue jobs select from with os__NAME__.
	images *runnerImages
	// workers bounds concurrent launches and reconciles in a tick; 0 or 1 is serial.
	workers int
}

func (s *scheduler) tick(ctx context.Context, now time.Time) error {
	// Once GitHub asks for a pause, the rest of the tick stops calling it.
	err := s.tickOnce(withRateGate(ctx), now)
	if ctx.Err() == nil && s.github != nil {
		s.github.ready.markTick(time.Now(), err)
	}
	return err
}

func (s *scheduler) tickOnce(ctx context.Context, now time.Time) error {
	tick := *s
	var state *operationState
	if s.operations != nil {
		s.operations.topology.RLock()
		defer s.operations.topology.RUnlock()
		state = s.operations.current.Load()
		tick.runtime, tick.pools, tick.hostLimits, tick.limit = state.runners.runtime, state.runners.pools, state.runners.hostLimits, state.policy.Limit
		tick.images = state.runners.images
	}
	if tick.workers > 1 {
		// Concurrent work still reaches each host one runtime call at a time; outbound workers take one claim at once.
		tick.runtime = &hostSerialRuntime{runnerRuntime: tick.runtime, hosts: make(map[string]*sync.Mutex)}
	}
	return tick.tickFleet(ctx, now, state)
}

// hostSerialRuntime serialises start and remove calls per host within one tick.
type hostSerialRuntime struct {
	runnerRuntime
	mu    sync.Mutex
	hosts map[string]*sync.Mutex
}

func (r *hostSerialRuntime) lock(host string) func() {
	r.mu.Lock()
	m := r.hosts[host]
	if m == nil {
		m = new(sync.Mutex)
		r.hosts[host] = m
	}
	r.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (r *hostSerialRuntime) startOn(ctx context.Context, host, name, jit string) error {
	defer r.lock(host)()
	return r.runnerRuntime.startOn(ctx, host, name, jit)
}

func (r *hostSerialRuntime) removeOn(ctx context.Context, host, name string) error {
	defer r.lock(host)()
	return r.runnerRuntime.removeOn(ctx, host, name)
}

// spawn runs f on a new goroutine once fewer than s.workers are busy, or
// inline when the scheduler is serial. Callers wait on wg before returning.
func (s *scheduler) spawn(slots chan struct{}, wg *sync.WaitGroup, f func()) {
	if s.workers <= 1 {
		f()
		return
	}
	slots <- struct{}{}
	wg.Add(1)
	go func() {
		defer func() {
			<-slots
			wg.Done()
		}()
		f()
	}()
}

func (s *scheduler) tickFleet(ctx context.Context, now time.Time, state *operationState) error {
	snapshots, err := s.runtime.snapshot(reapingStuckRunners(ctx))
	if err != nil {
		if s.panel != nil && ctx.Err() == nil {
			s.panel.capture(ctx, s.queue, nil, 0, s.limit, s.pools, err)
		}
		return err
	}
	active, err := s.queue.jobs(ctx, "WHERE runner_name IS NOT NULL")
	if err != nil {
		return err
	}
	states, err := readHostStates(ctx, s.queue.DB)
	if err != nil {
		return err
	}
	if err := s.reconcileFleet(ctx, active, snapshots, states, now); err != nil {
		return err
	}

	expired, err := s.queue.jobs(ctx, "WHERE runner_name IS NULL AND phase = ? AND changed_at <= ?",
		queued, now.Add(-queueExpiry).Unix())
	if err != nil {
		return err
	}
	for _, j := range expired {
		reason := "not started within 24 hours"
		if j.retryReason != "" {
			reason += "; last error: " + j.retryReason
		}
		if err := s.fail(ctx, j, now, reason); err != nil {
			return err
		}
	}
	// A rollup may fail or leave jobs for the next batch. Prune completed jobs
	// only when the ledger confirms they have been counted.
	prune := "DELETE FROM jobs WHERE runner_name IS NULL AND changed_at < ?"
	if err := rollupUsage(ctx, s.queue, s.pools, now); err != nil {
		if ctx.Err() == nil {
			slog.Warn("roll up usage statistics", "error", err)
		}
		prune += " AND phase <> ?"
	} else {
		prune += " AND (phase <> ? OR EXISTS (SELECT 1 FROM usage_rolled WHERE usage_rolled.job_id = jobs.job_id))"
	}
	_, err = s.queue.ExecContext(ctx, prune, now.Add(-7*24*time.Hour).Unix(), completed)
	if err != nil {
		return err
	}

	hosts, occupied := fleetCapacity(snapshots, states, active, s.hostLimits, s.limit)
	if s.metrics != nil {
		if err := s.metrics.capture(ctx, s.queue, hosts, occupied, s.limit, now); err != nil && ctx.Err() == nil {
			slog.Warn("collect fleet metrics", "error", err)
		}
	}
	if s.panel != nil {
		s.panel.capture(ctx, s.queue, hosts, occupied, s.limit, s.pools, nil)
	}
	if err := s.alerts.observe(ctx, s.queue, hosts, now, s.capped()); err != nil && ctx.Err() == nil {
		slog.Warn("derive fleet notifications", "error", err)
	}
	if warm, ok := unwrapRuntime(s.runtime).(warmRuntime); ok {
		// After launches, so warm runners take only the slots jobs left free, even when the tick ends early.
		unassigned := occupied
		defer func() {
			if ctx.Err() != nil {
				return
			}
			free := s.limit - unassigned
			for _, host := range hosts {
				free -= host.Occupied
			}
			warm.maintainWarm(ctx, hosts, free, now)
		}()
	}
	for _, host := range hosts {
		occupied += host.Occupied
	}
	// A host still waiting for its runner image counts here, so its jobs get a visible reason.
	if occupied >= s.limit || freeHost(hosts, "", false) == nil {
		return nil
	}
	// Read every waiting job: a repository cap or fairness can pass over the oldest ones.
	waiting, err := s.queue.jobs(ctx, "WHERE phase = 0 AND runner_name IS NULL AND retry_at <= ? ORDER BY sequence", now.Unix())
	if err != nil {
		return err
	}
	order := newFairOrder(waiting)
	perPool := make(map[[2]string]int)
	for _, j := range active {
		pool, _ := s.pools.forJob(j)
		perPool[[2]string{pool, j.repository}]++
		order.running[j.repository]++
	}
	// Only this goroutine touches the accounting. Each launch reserves its host,
	// pool and repository slot before it starts and returns it unless claimed.
	type launchResult struct {
		j          job
		host       *hostCapacity
		repository [2]string
		claimed    bool
		err        error
	}
	results := make(chan launchResult, s.limit)
	slots := make(chan struct{}, max(1, s.workers))
	var wg sync.WaitGroup
	settle := func() error {
		var first error
		for {
			select {
			case r := <-results:
				if !r.claimed {
					r.host.Occupied--
					perPool[r.repository]--
				}
				if r.err == nil {
					continue
				}
				if s.metrics != nil {
					s.metrics.launchErrors.Add(1)
				}
				// A claimed job holds a host slot until reconcile releases its lease, so it waits
				// only retryDelay; the release counts the failure and applies the backoff.
				delay := retryDelay
				if !r.claimed {
					delay = launchDelay(r.j.launchFailures)
				}
				if err := s.retryAfter(ctx, r.j, now, "provision runner", r.err, delay, !r.claimed); err != nil && first == nil {
					first = err
				}
			default:
				return first
			}
		}
	}
	parallel, held := s.workers > 1, false
	err = func() error {
		for attempts := 0; attempts < s.limit-occupied; {
			// Wait for a worker before looking at the queue, so the state and
			// rate-limit checks below see every launch that has finished.
			if parallel && !held {
				slots <- struct{}{}
				held = true
			}
			if err := settle(); err != nil {
				return err
			}
			j, more := order.next()
			if !more {
				return nil
			}
			if state != nil && s.operations.current.Load() != state {
				return nil
			}
			pool, err := s.pools.forJob(j)
			if err != nil {
				// An operator can fix the pool settings, and a failed job never re-queues, so keep waiting.
				if err := s.retry(ctx, j, now, "select worker pool", err); err != nil {
					return err
				}
				continue
			}
			repository := [2]string{pool, j.repository}
			if limit := s.repositoryCap(pool); limit > 0 && perPool[repository] >= limit {
				// A capped job waits untouched and must not hold back other repositories.
				continue
			}
			host, image, wait, err := s.placeJob(ctx, j, hosts, pool, now)
			if err != nil {
				// An unknown or disallowed OS name can be fixed in the catalogue, so keep waiting.
				if err := s.retry(ctx, j, now, "select runner image", err); err != nil {
					return err
				}
				continue
			}
			if err := s.waitForImage(ctx, j, wait); err != nil {
				return err
			}
			if host == nil {
				// No GitHub registration is spent until a host has the job's image.
				continue
			}
			if !s.github.backoffUntil(time.Now()).IsZero() {
				// GitHub asked for a pause; leave the remaining jobs untouched until it ends.
				return nil
			}
			attempts++
			order.running[j.repository]++
			host.Occupied++
			perPool[repository]++
			launch := func() {
				claimed, err := s.launch(withRunnerImage(ctx, image), j, host.Name, pool, now)
				results <- launchResult{j, host, repository, claimed, err}
			}
			if !parallel {
				launch()
				continue
			}
			held = false
			wg.Add(1)
			go func() {
				defer func() {
					<-slots
					wg.Done()
				}()
				launch()
			}()
		}
		return nil
	}()
	if held {
		<-slots
	}
	wg.Wait()
	if settled := settle(); err == nil {
		err = settled
	}
	return err
}

// repositoryCap is the most runners one repository may hold in pool; 0 is unlimited.
// capped reports whether any per-repository cap can hold a job back.
func (s *scheduler) capped() bool {
	if s.maxPerRepository > 0 {
		return true
	}
	for _, pool := range s.pools {
		if pool.MaxPerRepository > 0 {
			return true
		}
	}
	return false
}

func (s *scheduler) repositoryCap(pool string) int {
	return repositoryRunnerCap(s.pools[pool], s.maxPerRepository)
}

func repositoryRunnerCap(pool workerPool, fallback int) int {
	if pool.MaxPerRepository > 0 {
		return pool.MaxPerRepository
	}
	return fallback
}

// fairOrder yields waiting jobs FIFO within each repository, taking next the
// repository with the fewest runners; ties go to the longest-waiting job.
type fairOrder struct {
	queues  [][]fairJob
	running map[string]int
}

type fairJob struct {
	job
	position int
}

func newFairOrder(waiting []job) *fairOrder {
	o := &fairOrder{running: make(map[string]int)}
	index := make(map[string]int)
	for i, j := range waiting {
		n, ok := index[j.repository]
		if !ok {
			n = len(o.queues)
			index[j.repository] = n
			o.queues = append(o.queues, nil)
		}
		o.queues[n] = append(o.queues[n], fairJob{j, i})
	}
	return o
}

func (o *fairOrder) next() (job, bool) {
	best := -1
	for i, q := range o.queues {
		if len(q) == 0 {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		head, current := q[0], o.queues[best][0]
		if r, c := o.running[head.repository], o.running[current.repository]; r < c || r == c && head.position < current.position {
			best = i
		}
	}
	if best < 0 {
		return job{}, false
	}
	j := o.queues[best][0]
	o.queues[best] = o.queues[best][1:]
	return j.job, true
}

func availableHost(hosts []hostCapacity, pool string) *hostCapacity {
	return freeHost(hosts, pool, true)
}

// freeHost picks the least occupied usable host with a free slot; ready also requires its runner image.
func freeHost(hosts []hostCapacity, pool string, ready bool) *hostCapacity {
	var best *hostCapacity
	for i := range hosts {
		host := &hosts[i]
		if pool != "" && host.Pool != pool || host.Containers == nil || host.Err != nil || host.Cordoned || host.Occupied >= host.Limit ||
			ready && host.ImageWait != "" {
			continue
		}
		if best == nil || host.Occupied < best.Occupied {
			best = host
		}
	}
	return best
}

func (s *scheduler) reconcileFleet(ctx context.Context, active []job, snapshots []dockerHostSnapshot, states map[string]hostState, now time.Time) error {
	observations := make(map[string]dockerHostSnapshot, len(snapshots))
	seenOn := make(map[string]string)
	allObserved := true
	for _, host := range snapshots {
		observations[host.Name] = host
		allObserved = allObserved && host.Containers != nil
		for name := range host.Containers {
			seenOn[name] = host.Name
		}
	}
	for name, state := range states {
		if _, configured := observations[name]; !configured && state.untrackedSlots.Int64 > 0 {
			allObserved = false
		}
	}
	for _, j := range active {
		if _, configured := observations[j.runnerHost]; j.runnerHost != "" && !configured {
			allObserved = false
		}
	}

	owners := make(map[string]string, len(active))
	slots := make(chan struct{}, max(1, s.workers))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed error
	removed := make(map[string][]string)
	err := func() error {
		for i := range active {
			j := &active[i]
			seen := seenOn[j.runnerName]
			if j.runnerHost == "" && seen != "" {
				// Adopt runners that predate persisted Docker host ownership.
				_, err := s.queue.ExecContext(ctx, "UPDATE jobs SET runner_host = ? WHERE job_id = ?", seen, j.id)
				if err != nil {
					return err
				}
				j.runnerHost = seen
			}
			owners[j.runnerName] = j.runnerHost
			if j.retryAt > now.Unix() || (seen != "" && j.runnerHost != seen) {
				continue
			}
			host := observations[j.runnerHost]
			if host.Containers == nil && (j.runnerHost != "" || !allObserved) {
				continue
			}
			mu.Lock()
			stop := failed
			mu.Unlock()
			if stop != nil {
				return nil
			}
			leased, state, detail := *j, host.Containers[j.runnerName], host.RunnerDiagnostics[j.runnerName]
			if killed, checked := host.OOMKilled[j.runnerName]; checked {
				recordJobOOM(ctx, s.queue.DB, j.id, killed)
			}
			s.spawn(slots, &wg, func() {
				gone, err := s.reconcile(ctx, leased, state, detail, now)
				if err != nil {
					err = s.retry(ctx, leased, now, "reconcile runner", err)
				}
				mu.Lock()
				defer mu.Unlock()
				if gone {
					removed[leased.runnerHost] = append(removed[leased.runnerHost], leased.runnerName)
				}
				if err != nil && failed == nil {
					failed = err
				}
			})
		}
		return nil
	}()
	wg.Wait()
	if err == nil {
		err = failed
	}
	if err != nil {
		return err
	}
	for host, names := range removed {
		for _, name := range names {
			delete(observations[host].Containers, name)
		}
	}

	for _, host := range snapshots {
		if host.Containers == nil {
			continue
		}
		var untracked int
		for name, state := range host.Containers {
			if owners[name] != host.Name && runnerOccupiesSlot(state) {
				untracked++
			}
		}
		if previous := states[host.Name].untrackedSlots; !previous.Valid || previous.Int64 != int64(untracked) {
			if err := s.queue.observeHostSlots(ctx, host.Name, untracked); err != nil {
				return err
			}
		}
	}
	return nil
}

const imageWaitPrefix = "waiting for runner image "

// waitForImage records why a job with no ready host waits for a runner image (from placeJob), and clears
// that reason once a ready host is found or no host waits for an image any more.
func (s *scheduler) waitForImage(ctx context.Context, j job, reason string) error {
	if reason == j.retryReason || reason == "" && !strings.HasPrefix(j.retryReason, imageWaitPrefix) {
		return nil
	}
	_, err := s.queue.ExecContext(ctx, "UPDATE jobs SET retry_reason = ? WHERE job_id = ? AND runner_name IS NULL AND retry_reason = ?",
		reason, j.id, j.retryReason)
	return err
}

func (s *scheduler) fail(ctx context.Context, j job, now time.Time, reason string) error {
	failed, err := s.queue.fail(ctx, j.id, now, reason)
	if failed {
		slog.Warn("job failed", "repository", j.repository, "job", j.id, "reason", reason)
		s.alerts.jobFailed(j, now, reason)
	}
	return err
}

func (s *scheduler) retry(ctx context.Context, j job, now time.Time, operation string, cause error) error {
	return s.retryAfter(ctx, j, now, operation, cause, retryDelay, false)
}

// retryAfter makes j wait at least delay; failed counts one more failed launch toward launchDelay.
func (s *scheduler) retryAfter(ctx context.Context, j job, now time.Time, operation string, cause error, delay time.Duration, failed bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	slog.Warn(operation, "repository", j.repository, "job", j.id, "error", cause)
	retryAt := now.Add(delay)
	var apiErr *githubError
	if errors.As(cause, &apiErr) && apiErr.retryAt.After(retryAt) {
		retryAt = apiErr.retryAt
	}
	increment := 0
	if failed {
		increment = 1
	}
	_, err := s.queue.ExecContext(ctx, "UPDATE jobs SET retry_at = ?, retry_reason = ?, launch_failures = launch_failures + ? WHERE job_id = ?",
		retryAt.Unix(), retryMessage(operation, cause), increment, j.id)
	return err
}

// launchDelay is how long a queued job waits after failures consecutive launches that no runner
// picked up. It doubles from retryDelay up to maxLaunchDelay, so a single failure retries as fast
// as before while a broken runner image cannot spend the GitHub API rate limit.
func launchDelay(failures int) time.Duration {
	delay := retryDelay
	for range failures {
		if delay >= maxLaunchDelay {
			break
		}
		delay *= 2
	}
	return min(delay, maxLaunchDelay)
}

func retryMessage(operation string, cause error) string {
	text := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, cause.Error())
	detail := []rune(strings.Join(strings.Fields(text), " "))
	if len(detail) > 240 {
		detail = append(detail[:240], '…')
	}
	return operation + ": " + string(detail)
}

func (s *scheduler) launch(ctx context.Context, j job, host, pool string, now time.Time) (bool, error) {
	err := s.github.checkRepositoryAccess(ctx, j)
	var token string
	if err == nil {
		token, err = s.github.token(ctx, j)
	}
	if errors.Is(err, errRepositoryOutsideInstallation) {
		return false, s.discardOutsideInstallation(ctx, j)
	}
	if err != nil {
		return false, err
	}
	repo, eligible, err := s.github.eligible(ctx, j, token)
	if err != nil {
		return false, err
	}
	if !eligible {
		// Runquill will never run this job, so record why instead of leaving it looking finished.
		return false, s.fail(ctx, j, now, "repository is no longer eligible: "+s.github.ineligibility(j, repo))
	}
	policy := s.pools[pool]
	if policy.Repositories != nil {
		name, valid := ownedRepositoryPath(repo.FullName, s.github.owner)
		if !valid || !policy.allows(name) {
			return false, fmt.Errorf("GitHub repository %q is not allowed to use worker pool %q", repo.FullName, pool)
		}
	}
	observed, err := s.github.jobDetails(ctx, j, token)
	if err != nil {
		return false, err
	}
	if observed.Status != "queued" {
		if p, known := phases[observed.Status]; known {
			return false, s.queue.advanceGitHubJob(ctx, j.id, p, now, observed)
		}
		return false, fmt.Errorf("waiting for GitHub to queue job, status %q", observed.Status)
	}
	restrictCache := policy.cacheWritesRestricted(repo)
	var run workflowRun
	if !repo.Private || restrictCache {
		// Only public repositories and pools that restrict cache writes pay for this lookup.
		if run, err = s.github.workflowRun(ctx, j, observed.RunID, token); err != nil {
			return false, err
		}
	}
	if !repo.Private {
		// eligible already refused public repositories unless allowed.
		if reason := run.forkReason(j); reason != "" {
			return false, s.fail(ctx, j, now, reason)
		}
		// The runner takes any queued job with its label, and public labels are predictable, so a
		// fork job could claim it. Errors wait with the launch backoff until the repository is safe.
		adminToken := token
		if s.github.runnerGroupID > 0 {
			adminToken = "" // runner group installations read repositories with an Actions-only token
		}
		if err := s.github.guardPublicRepository(ctx, j, token, adminToken); err != nil {
			return false, err
		}
	}
	scratch := restrictCache && !run.writesCache(j, repo.DefaultBranch)
	if restrictCache && !scratch {
		// shortcut: a job queued after this check can still take the runner; nothing closes that race yet.
		if scratch, err = s.github.cacheLabelContested(ctx, j, run.ID, repo.DefaultBranch, token); err != nil {
			return false, err
		}
	}
	if s.github.runnerGroupID > 0 {
		token, err = s.github.organizationJobToken(ctx, j, true)
		if errors.Is(err, errRepositoryOutsideInstallation) {
			return false, s.discardOutsideInstallation(ctx, j)
		}
		if err != nil {
			return false, err
		}
	}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return false, err
	}
	name := fmt.Sprintf("runquill-%d-%s", j.id, hex.EncodeToString(nonce[:]))
	jitRequested := time.Now().UnixMilli()
	claim, err := s.queue.ExecContext(ctx, `
        UPDATE jobs SET runner_name = ?, runner_host = ?, launched_at = ?,
            jit_requested_ms = ?, jit_registered_ms = NULL, runner_launch_ms = NULL
        WHERE job_id = ? AND phase = ? AND runner_name IS NULL
        AND NOT EXISTS (SELECT 1 FROM docker_hosts WHERE host = ? AND cordoned = 1)`,
		name, host, now.Unix(), jitRequested, j.id, queued, host)
	if err != nil {
		return false, err
	}
	count, err := claim.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	runnerID, jit, err := s.github.provision(ctx, j, name, token)
	if err != nil {
		return true, err
	}
	jitRegistered := time.Now().UnixMilli()
	_, err = s.queue.ExecContext(ctx, `UPDATE jobs SET runner_id = ?,
        provisioned_at = coalesce(provisioned_at, ?),
        github_created_at = coalesce(github_created_at, nullif(?, 0)),
        jit_registered_ms = ? WHERE job_id = ?`,
		runnerID, jitRegistered/1000, githubTimestamp(observed.CreatedAt), jitRegistered, j.id)
	if err != nil {
		return true, err
	}
	var current phase
	if err := s.queue.QueryRowContext(ctx, "SELECT phase FROM jobs WHERE job_id = ?", j.id).Scan(&current); err != nil {
		return true, err
	}
	if current == queued {
		start := withRunnerRepository(ctx, j.repositoryID)
		if scratch {
			start = withScratchCache(start)
		}
		if err := s.runtime.startOn(start, host, name, jit); err != nil {
			return true, err
		}
		runnerLaunch := time.Now().UnixMilli()
		if s.metrics != nil {
			s.metrics.starts.Add(1)
		}
		slog.Info("runner started", "runner", name, "runner_host", host, "repository", j.repository, "job", j.id, "scratch_cache", scratch)
		_, err = s.queue.ExecContext(ctx, `UPDATE jobs SET retry_at = 0, retry_reason = '', runner_launch_ms = ?
            WHERE job_id = ? AND runner_name = ?`, runnerLaunch, j.id, name)
		return true, err
	}
	return true, nil
}

// reconcile checks one leased runner whose observed runtime state is state.
// removed reports that its runtime was deleted, even when a later step fails.
func (s *scheduler) reconcile(ctx context.Context, j job, state string, detail jobDiagnostic, now time.Time) (removed bool, err error) {
	outsideInstallation := false
	if state == "running" || state == "pending" || state == "paused" || state == "restarting" {
		switch j.phase {
		case completed:
			if now.Unix() < j.changedAt+int64(completionGrace.Seconds()) {
				return false, nil
			}
		default:
			lastSeen := max(j.launchedAt, j.changedAt)
			if now.Unix() < lastSeen+int64(idleTimeout.Seconds()) {
				return false, nil
			}
			token, err := s.github.token(ctx, j)
			if errors.Is(err, errRepositoryOutsideInstallation) {
				outsideInstallation = true
				break
			}
			if err != nil {
				return false, err
			}
			observed, err := s.github.jobDetails(ctx, j, token)
			if githubNotFound(err) {
				return false, s.queue.advance(ctx, j.id, completed, now)
			}
			if err != nil {
				return false, err
			}
			if observed.Status == "completed" {
				return false, s.queue.advanceGitHubJob(ctx, j.id, completed, now, observed)
			}
			if observed.Status == "in_progress" || j.phase == inProgress {
				// Recheck periodically when GitHub misses a lifecycle webhook.
				_, err := s.queue.ExecContext(ctx, `UPDATE jobs SET phase = ?, changed_at = ?,
                    github_created_at = coalesce(github_created_at, nullif(?, 0)),
                    github_started_at = coalesce(github_started_at, nullif(?, 0))
                    WHERE job_id = ? AND phase < ?`, inProgress, now.Unix(),
					githubTimestamp(observed.CreatedAt), githubTimestamp(observed.StartedAt), j.id, completed)
				return false, err
			}
		}
	}
	if state != "" {
		err := s.runtime.removeOn(ctx, j.runnerHost, j.runnerName)
		switch {
		case errors.Is(err, errRunnerRemovalPending):
			// The runtime finishes the removal on its own. Release the lease so the job can
			// use another runner; the snapshot still counts this one's slot until it is gone.
			slog.Warn("runner removal still in progress", "runner", j.runnerName, "repository", j.repository, "job", j.id, "error", err)
		case err != nil:
			return false, err
		default:
			removed = true
		}
	}
	if j.phase != completed && !outsideInstallation {
		// A runner can exit before GitHub has sent its final job event, including in polling-only mode.
		token, err := s.github.token(ctx, j)
		var observed workflowJob
		if err == nil {
			observed, err = s.github.jobDetails(ctx, j, token)
		}
		outsideInstallation = errors.Is(err, errRepositoryOutsideInstallation)
		if err != nil && !githubNotFound(err) && !outsideInstallation {
			return removed, err
		}
		if observed.Status == "completed" || githubNotFound(err) {
			if err := s.queue.advanceGitHubJob(ctx, j.id, completed, now, observed); err != nil {
				return removed, err
			}
		} else if observed.Status == "in_progress" {
			if err := s.queue.advanceGitHubJob(ctx, j.id, inProgress, now, observed); err != nil {
				return removed, err
			}
			_, err := s.queue.ExecContext(ctx, `UPDATE jobs SET retry_at = ?,
                retry_reason = 'waiting for GitHub to finish the job after the runner exited'
                WHERE job_id = ? AND phase < ?`, now.Add(retryDelay).Unix(), j.id, completed)
			return removed, err
		}
	}
	if !outsideInstallation || s.github.runnerGroupID > 0 {
		if err := s.github.remove(ctx, j); err != nil {
			return removed, err
		}
	}
	if outsideInstallation {
		if err := s.discardOutsideInstallation(ctx, j); err != nil {
			return removed, err
		}
	}
	previousRunner := ""
	if detail.Message != "" && detail.Code != "pod_terminating" {
		previousRunner = "Previous runner · " + detail.Message
	}
	// A runner released while its job is still queued never picked it up, so the next launch waits longer.
	// RETURNING reads the finished job's stage timestamps in the same statement, so each job is observed once.
	released := j
	err = s.queue.QueryRowContext(ctx, `UPDATE jobs SET runner_name = NULL, runner_host = NULL, runner_id = NULL,
        launched_at = NULL, retry_at = ?,
        runner_cleaned_at = CASE WHEN phase = ? THEN max(changed_at, ?) END,
        runner_cleaned_host = CASE WHEN phase = ? THEN runner_host END,
        retry_reason = CASE WHEN phase = ? THEN coalesce(nullif(?, ''), nullif(retry_reason, ''),
            'runner became unavailable while the job was queued') ELSE '' END,
        launch_failures = CASE WHEN phase = ? THEN launch_failures + 1 ELSE 0 END
        WHERE job_id = ? AND runner_name = ?
        RETURNING phase, coalesce(discovered_at, 0), coalesce(github_started_at, 0), coalesce(github_completed_at, 0),
            `+jobLaunchTimingColumns, now.Add(launchDelay(j.launchFailures)).Unix(), completed, time.Now().Unix(), completed, queued, previousRunner,
		queued, j.id, j.runnerName).Scan(&released.phase, &released.discoveredAt, &released.githubStartedAt, &released.githubCompletedAt,
		&released.jitRequestedMS, &released.jitRegisteredMS, &released.runnerLaunchMS)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return removed, err
	}
	if s.metrics != nil {
		s.metrics.releases.Add(1)
		if err == nil && released.phase == completed {
			pool, _ := s.pools.forJob(j)
			s.metrics.observeJob(pool, jobTiming(released, now))
		}
	}
	slog.Info("runner removed", "runner", j.runnerName, "repository", j.repository, "job", j.id)
	return removed, nil
}

func (s *scheduler) discardOutsideInstallation(ctx context.Context, j job) error {
	// A later approval can rediscover the same GitHub job if it is still queued.
	_, err := s.queue.ExecContext(ctx, `DELETE FROM jobs
        WHERE job_id = ? AND coalesce(runner_name, '') = ? AND phase < ?`, j.id, j.runnerName, completed)
	if err == nil {
		slog.Info("discarded job outside GitHub App installation", "repository", j.repository, "job", j.id)
	}
	return err
}
