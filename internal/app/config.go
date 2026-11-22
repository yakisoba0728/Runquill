package app

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	owner, appID, secret, keyPath, runnerImage string
	repositoryScope                            string
	organization, allowPublic                  bool
	runnerGroupID                              int64
	limit, maxPerRepository, workers           int
	pollInterval                               time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		owner:       strings.TrimSpace(os.Getenv("RUNQUILL_OWNER")),
		appID:       strings.TrimSpace(os.Getenv("RUNQUILL_APP_ID")),
		secret:      os.Getenv("RUNQUILL_WEBHOOK_SECRET"),
		keyPath:     os.Getenv("RUNQUILL_KEY_FILE"),
		runnerImage: env("RUNQUILL_RUNNER_IMAGE", "runquill-runner:local"),
	}
	var missing []string
	for _, setting := range []struct{ name, value string }{
		{"RUNQUILL_OWNER", cfg.owner}, {"RUNQUILL_APP_ID", cfg.appID},
		{"RUNQUILL_KEY_FILE", cfg.keyPath},
	} {
		if setting.value == "" {
			missing = append(missing, setting.name)
		}
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("set %s", strings.Join(missing, ", "))
	}
	limit, err := runnerLimit()
	if err != nil {
		return cfg, err
	}
	cfg.limit = limit
	cfg.maxPerRepository, err = strconv.Atoi(env("RUNQUILL_MAX_PER_REPOSITORY", "0"))
	if err != nil || cfg.maxPerRepository < 0 || cfg.maxPerRepository > maxPerRepositoryLimit {
		return cfg, fmt.Errorf("RUNQUILL_MAX_PER_REPOSITORY must be an integer from 0 (unlimited) to %d", maxPerRepositoryLimit)
	}
	cfg.workers, err = strconv.Atoi(env("RUNQUILL_SCHEDULER_WORKERS", "4"))
	if err != nil || cfg.workers < 1 || cfg.workers > 32 {
		return cfg, errors.New("RUNQUILL_SCHEDULER_WORKERS must be an integer from 1 (serial) to 32")
	}
	cfg.repositoryScope, err = parseRepositoryScope(os.Getenv(repositoryScopeEnv))
	if err != nil {
		return cfg, fmt.Errorf("%s: %w", repositoryScopeEnv, err)
	}
	cfg.organization, cfg.runnerGroupID, err = parseGitHubTarget(os.Getenv(ownerTypeEnv), os.Getenv(runnerGroupEnv))
	if err != nil {
		return cfg, err
	}
	if cfg.allowPublic, err = parseAllowPublic(os.Getenv(allowPublicEnv)); err != nil {
		return cfg, err
	}
	if cfg.organization && !setupLogin.MatchString(cfg.owner) {
		return cfg, errors.New("set RUNQUILL_OWNER to the GitHub organization login")
	}
	cfg.pollInterval, err = time.ParseDuration(env("RUNQUILL_POLL_INTERVAL", "30s"))
	if err != nil || cfg.pollInterval < 0 {
		return cfg, errors.New("RUNQUILL_POLL_INTERVAL must be a duration such as 30s, or 0 to disable polling")
	}
	if cfg.pollInterval == 0 && cfg.secret == "" {
		return cfg, errors.New("set RUNQUILL_WEBHOOK_SECRET or enable GitHub polling")
	}
	return cfg, nil
}

func runnerLimit() (int, error) {
	limit, err := strconv.Atoi(env("RUNQUILL_MAX_RUNNERS", "2"))
	if err != nil || limit < 1 {
		return 0, errors.New("RUNQUILL_MAX_RUNNERS must be a positive integer")
	}
	return limit, nil
}

func (cfg config) github() (*github, error) {
	keyData, err := os.ReadFile(cfg.keyPath)
	if err != nil {
		return nil, fmt.Errorf("read GitHub App key: %w", err)
	}
	key, err := parseKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App key: %w", err)
	}
	return &github{client: &http.Client{Timeout: 10 * time.Second}, base: githubAPIBase,
		appID: cfg.appID, key: key, owner: cfg.owner, repositoryScope: cfg.repositoryScope,
		organization: cfg.organization, runnerGroupID: cfg.runnerGroupID, allowPublic: cfg.allowPublic}, nil
}

// allowPublicEnv lets jobs from public repositories run. Only the deployment can set it.
const allowPublicEnv = "RUNQUILL_ALLOW_PUBLIC_REPOSITORIES"

func parseAllowPublic(value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return false, nil
	}
	allow, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", allowPublicEnv)
	}
	return allow, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
