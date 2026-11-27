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
}

export interface JobTiming {
  runquill_queue_seconds?: number | null;
}

export interface Job {
  id: number;
  repository: string;
  status: string; // WAITING, RETRY, RUNNING, OFFLINE, CLEANUP
  pool?: string;
  runner?: string;
  runner_host?: string;
  queue_position?: number;
  retry_at?: string;
  retry_reason?: string;
  diagnostic?: { code?: string; message?: string };
  timing?: JobTiming;
}

export interface Runner {
  name: string;
  job_id?: number;
  host?: string;
  pool?: string;
  runtime_state?: string;
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
  fleet?: Fleet;
}
