// The parts of the server's JSON the panel reads (api/openapi.yaml). Only the fields in use, written by hand.

export type Role = "viewer" | "operator" | "admin";

export interface User {
  login: string;
  role: Role;
}

export interface Host {
  name: string;
  pool?: string;
  status: string; // UP, OFFLINE, UNAVAILABLE
  cordoned?: boolean;
  slot_limit: number;
  occupied_slots: number;
  reserved_slots: number;
  image_status?: string; // ready, pulling, failed, missing
  last_seen?: string;
  error?: string;
  warm?: number;
  warm_target?: number;
  enrollment_available?: boolean;
}

/** status.go statusJobTiming: seconds, milliseconds and the moments behind them. */
export interface JobTiming {
  github_created_at?: string | null;
  discovered_at?: string | null;
  discovery_source?: string;
  provisioned_at?: string | null;
  github_started_at?: string | null;
  github_completed_at?: string | null;
  jit_requested_at?: string | null;
  jit_registered_at?: string | null;
  runner_launch_at?: string | null;
  creation_to_discovery_seconds?: number | null;
  runquill_queue_seconds?: number | null;
  provision_to_start_seconds?: number | null;
  scheduler_wait_millis?: number | null;
  jit_registration_millis?: number | null;
  runtime_launch_millis?: number | null;
  launch_to_github_start_seconds?: number | null;
  execution_seconds?: number | null;
}

/** A job in the snapshot: fleet.jobs (with a status), or recent (cleaned up, no status). */
export interface Job {
  id: number;
  repository: string;
  status?: string; // WAITING, RETRY, RUNNING, OFFLINE, CLEANUP; absent in recent
  pool?: string;
  pool_error?: string;
  runner?: string;
  runner_host?: string;
  runner_label?: string;
  runner_os?: string;
  runner_image?: string;
  url?: string;
  queue_position?: number;
  retry_at?: string;
  retry_reason?: string;
  diagnostic?: { code?: string; message?: string };
  timing?: JobTiming;
  finished_at?: string;
  cleaned_at?: string;
}

export interface Runner {
  name: string;
  job_id?: number;
  host?: string;
  pool?: string;
  runtime_state?: string;
  runner_os?: string;
  runner_image?: string;
}

/** GET /api/jobs rows; GET /api/jobs/{id} adds timing and resources. */
export interface HistoryJob {
  id: number;
  repository: string;
  outcome: string; // waiting, running, finished, failed
  pool?: string;
  host?: string;
  runner_label?: string;
  runner_os?: string;
  url?: string;
  queued_at?: string;
  started_at?: string;
  finished_at?: string;
  cleaned_at?: string;
  wait_seconds?: number;
  failure?: string;
  diagnostic?: { code?: string; message?: string };
  retry_at?: string;
  retry_reason?: string;
  timing?: JobTiming;
  resources?: JobResources | null;
}

export interface JobResources {
  samples: number;
  cpu_limit?: number;
  cpu_limit_max?: number;
  cpu_peak?: number;
  cpu_average?: number;
  memory_limit_mib?: number;
  memory_limit_max_mib?: number;
  memory_peak_mib?: number;
  oom_killed?: boolean;
}

export interface UsageTotals {
  jobs: number;
  failed: number;
  runner_seconds: number;
  wait_count: number;
  wait_avg_seconds: number;
  wait_p50_seconds: number;
  wait_p95_seconds: number;
}

export interface UsageReport {
  from: string;
  to: string;
  group: string;
  truncated?: boolean;
  totals: UsageTotals;
  series: (UsageTotals & { key: string })[];
}

export interface RightsizingResource {
  limit: number;
  peak_max: number;
  peak_ratio?: number;
  verdict: string; // tight, oversized, ok, too_few_jobs, unlimited
}

export interface RightsizingReport {
  group: string;
  days: number;
  truncated?: boolean;
  groups: { key: string; jobs: number; sampled_jobs: number; oom_kills: number; cpu?: RightsizingResource; memory?: RightsizingResource }[];
}

/** GET /api/hosts/resources (panel_resources.go). */
export interface ResourceReport {
  disk_measuring?: boolean;
  disk_measured_at?: string;
  hosts?: {
    host: string;
    unavailable?: string; // agent, kubernetes
    error?: string;
    cpus?: number;
    memory_bytes?: number;
    reserved?: { runners: number; runner_cpus: number; runner_memory_bytes: number; cpus: number; memory_bytes: number };
    usage?: { cpus: number; memory_bytes: number };
    disk?: { images: number; image_bytes: number; cache_volumes: number; cache_bytes?: number | null };
    disk_error?: string;
  }[];
}

/** GET /api/elastic (elastic.go report). */
export interface ElasticReport {
  mode: string; // off, observe, apply
  interval_seconds?: number;
  hosts?: {
    host: string; cpus: number; memory_mib: number; held_slots: number; surplus_cpus: number; lent_cpus: number;
    surplus_memory_mib: number; extra_memory_mib: number; memory_overcommitted?: boolean;
  }[];
  decisions?: {
    at: string; host: string; runner: string; resource: string; from: number; to: number; reason?: string;
    applied?: boolean; forced?: boolean; error?: string;
  }[];
}

export interface Fleet {
  slot_limit: number;
  backend?: string;
  repository_scope?: string;
  public_repositories?: boolean;
  docker_error?: string;
  hosts?: Host[];
  jobs?: Job[];
  runners?: {
    known_running?: number;
    occupied_slots?: number;
    reserved_slots?: number;
    unassigned_reserved_slots?: number;
  };
}

export interface Snapshot {
  sampled_at?: string;
  webhook?: boolean;
  polling?: boolean;
  last_webhook?: string;
  last_poll?: string;
  poll_failed?: boolean;
  first_job_seen?: boolean;
  first_job_clean?: { id: number; url?: string } | null;
  runner_inventory?: Runner[];
  recent?: Job[];
  fleet?: Fleet;
}
