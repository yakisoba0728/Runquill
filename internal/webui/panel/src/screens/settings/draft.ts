// The settings form's model (design §3.6), kept apart from the screen so the rules are tested: the editable draft made
// from GET /api/settings, how many list entries differ from it, and the PUT body, assembled as v1 settings.js does.
import { t } from "../../i18n";

/** A worker pool as the server stores it; fields this editor does not show (max_per_repository, cache_writes) pass through. */
export interface WorkerPool {
  hosts?: string[] | null;
  repositories?: string[] | null;
  cpus?: number;
  memory_mib?: number;
  image?: string;
  privileged?: boolean;
  cache?: boolean;
  images?: string[];
  warm?: number;
  warm_max_idle_minutes?: number;
  [field: string]: unknown;
}

export interface Policy {
  revision: number;
  slot_limit: number;
  poll_interval: string;
  runner_image: string;
  docker_hosts?: string[] | null;
  host_limits?: Record<string, number> | null;
  worker_pools?: Record<string, WorkerPool> | null;
}

export interface Settings {
  backend: string;
  webhook_enabled: boolean;
  policy: Policy;
  runner_images?: { label?: string }[] | null;
}

export interface Worker {
  key: string;
  address: string;
  limit: string;
}

export interface Pool {
  key: string;
  name: string;
  cpus: string;
  memory: string;
  warm: string;
  warmIdle: string;
  image: string;
  /** Comma-separated, as typed. */
  images: string;
  /** One per line (commas work too), as typed. */
  hosts: string;
  restrict: boolean;
  repositories: string;
  privileged: boolean;
  cache: boolean;
  /** The pool as loaded, so fields the editor does not show are saved back unchanged. */
  original: WorkerPool;
}

/** Text fields: useFormDirty compares them with Object.is. */
export interface FleetValues {
  slot_limit: string;
  poll_interval: string;
  runner_image: string;
  [field: string]: unknown;
}

export interface Draft {
  revision: number;
  fleet: FleetValues;
  workers: Worker[];
  pools: Pool[];
}

// v1 shows an unset (or zero) pool number as an empty box.
const text = (value: unknown) => (value ? String(value) : "");

/** The form's values for a settings answer. Call once per answer and use it for both the baseline and the edit. */
export function draftOf({ policy }: Settings): Draft {
  return {
    revision: policy.revision,
    fleet: { slot_limit: String(policy.slot_limit ?? ""), poll_interval: policy.poll_interval ?? "", runner_image: policy.runner_image ?? "" },
    workers: (policy.docker_hosts ?? []).map((address, i) => ({ key: "w" + i, address, limit: String(policy.host_limits?.[address] ?? "") })),
    pools: Object.entries(policy.worker_pools ?? {}).map(([name, pool], i) => ({
      key: "p" + i,
      name,
      cpus: text(pool.cpus),
      memory: text(pool.memory_mib),
      warm: text(pool.warm),
      warmIdle: text(pool.warm_max_idle_minutes),
      image: pool.image ?? "",
      images: (pool.images ?? []).join(", "),
      hosts: (pool.hosts ?? []).join("\n"),
      restrict: Array.isArray(pool.repositories),
      repositories: (pool.repositories ?? []).join("\n"),
      privileged: pool.privileged !== false,
      cache: pool.cache === true,
      original: pool,
    })),
  };
}

/** Entries added, removed or changed between two lists keyed by key (each counts once). */
export function listChanges<T extends { key: string }>(saved: readonly T[], current: readonly T[]) {
  const before = new Map(saved.map((entry) => [entry.key, JSON.stringify(entry)]));
  let n = 0;
  for (const entry of current) {
    if (before.get(entry.key) !== JSON.stringify(entry)) n++;
    before.delete(entry.key);
  }
  return n + before.size;
}

// Pool images are runner image catalogue names (runner_images.go runnerImageName).
export const runnerImageName = /^[a-z0-9][a-z0-9.-]{0,39}$/;
export const maxRunnerImages = 64;

const list = (value: string) => value.split(/[,\n]/).map((part) => part.trim()).filter(Boolean);

/** The allowed runner OS names; undefined (absent) allows every name. */
function poolImages(value: string, pool: string) {
  const names = list(value);
  if (!names.length) return undefined;
  const invalid = names.find((name) => !runnerImageName.test(name));
  if (invalid) throw new Error(t("Pool {pool}: {name} is not a runner OS name. Use 1 to 40 lowercase letters, digits, dots or hyphens.", { pool, name: invalid }));
  if (names.length > maxRunnerImages) throw new Error(t("Pool {pool} may allow at most {n} runner OS names.", { pool, n: maxRunnerImages }));
  return names;
}

/** The PUT /api/settings body (v1 settings.js); throws an Error with a translated message for a problem it can see. */
export function settingsBody({ revision, fleet, workers, pools }: Draft) {
  const rows = pools.map((pool) => ({ ...pool, name: pool.name.trim() }));
  if (new Set(rows.map((pool) => pool.name)).size !== rows.length) throw new Error(t("Use a different name for each worker pool."));
  const hosts = workers.map((worker) => ({ address: worker.address.trim(), limit: worker.limit.trim() }));
  return {
    revision,
    slot_limit: Number(fleet.slot_limit),
    poll_interval: fleet.poll_interval.trim(),
    runner_image: fleet.runner_image.trim(),
    docker_hosts: hosts.map((row) => row.address),
    host_limits: Object.fromEntries(hosts.filter((row) => row.limit).map((row) => [row.address, Number(row.limit)])),
    worker_pools: Object.fromEntries(rows.map((row) => [row.name, {
      ...row.original,
      hosts: list(row.hosts),
      repositories: row.restrict ? list(row.repositories) : null,
      cpus: Number(row.cpus),
      memory_mib: Number(row.memory),
      image: row.image.trim(),
      // An absent value means the privileged default; keep it absent unless it changes.
      privileged: row.privileged ? (row.original.privileged === false ? true : row.original.privileged) : false,
      cache: row.cache,
      warm: Number(row.warm) || undefined,
      warm_max_idle_minutes: Number(row.warmIdle) || undefined,
      images: poolImages(row.images, row.name),
    }])),
  };
}
