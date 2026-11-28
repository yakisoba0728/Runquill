// Settings (design §3.6, §4.7): the operational policy from GET/PUT /api/settings, applied without a restart. Admins
// only; the server registers the route only with panel operations, so a 404 shows as an error. Ported from v1
// settings.js. Unsaved changes: the SaveBar counts them, warns before the tab closes, and holds the router's leave guard,
// so leaving shakes the bar and offers "discard and leave" (no confirm dialog, a decided change from v1).
import {
  Accordion, Button, Checkbox, Field, Fieldset, FormSection, Input, SaveBar, Skeleton, Textarea, useFormDirty,
} from "@yakihyuk0728/stratum-ui";
import { useEffect, useMemo, useRef, useState } from "react";
import { errorText, request, type ApiError } from "../../api";
import { useScreenCommands, type ScreenCommand } from "../../commands";
import { Notice, Page } from "../../components";
import { confirm } from "../../dialogs";
import { fill, t } from "../../i18n";
import { stratum } from "../../i18n/stratum";
import { useLeaveGuard } from "../../router";
import { draftOf, listChanges, settingsBody, type Draft, type Pool, type Settings, type Worker } from "./draft";

let added = 0;
const newKey = () => "new" + ++added;
const blankPool = (): Pool => ({
  key: newKey(), name: "", cpus: "", memory: "", warm: "", warmIdle: "", image: "", images: "", hosts: "",
  restrict: false, repositories: "", privileged: true, cache: false, original: {},
});

const focusLater = (selector: string) => requestAnimationFrame(() => document.querySelector<HTMLElement>(selector)?.focus());

export default function SettingsScreen() {
  const labels = useMemo(stratum, []);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [loadError, setLoadError] = useState<ApiError | null>(null);
  const [loading, setLoading] = useState(true);
  const fleet = useFormDirty({ slot_limit: "", poll_interval: "", runner_image: "" });
  const [saved, setSaved] = useState<Draft | null>(null);
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [pools, setPools] = useState<Pool[]>([]);
  const [open, setOpen] = useState<string[]>([]);
  const [guard, setGuard] = useState(0);
  const leave = useRef<(() => void) | null>(null);
  const focusing = useRef(false);

  const count = saved ? fleet.count + listChanges(saved.workers, workers) + listChanges(saved.pools, pools) : 0;

  const populate = (next: Settings) => {
    const draft = draftOf(next);
    fleet.commit(draft.fleet);
    fleet.setValues(draft.fleet);
    setSaved(draft);
    setWorkers(draft.workers);
    setPools(draft.pools);
    setOpen(draft.pools.map((pool) => pool.key));
    setSettings(next);
  };

  const load = async () => {
    setLoading(true);
    setLoadError(null);
    try {
      populate(await request<Settings>("./api/settings"));
    } catch (error) {
      if ((error as ApiError).status !== 401) setLoadError(error as ApiError);
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => void load(), []);

  const revert = () => {
    if (!saved) return [];
    setWorkers(saved.workers);
    setPools(saved.pools);
    setOpen(saved.pools.map((pool) => pool.key));
    return fleet.revert();
  };

  const reload = async (returnFocus?: HTMLElement | null) => {
    if (count && !await confirm({ title: t("Discard your unsaved settings changes?"), confirmLabel: t("Discard changes"), danger: true, returnFocus })) return;
    await load();
  };

  const save = async () => {
    if (!saved) return;
    // A problem found here or by the server shows in the bar's error tone; the edits stay.
    const body = settingsBody({ revision: saved.revision, fleet: fleet.values, workers, pools });
    try {
      populate(await request<Settings>("./api/settings", { method: "PUT", data: body, timeout: 10000 }));
    } catch (error) {
      throw new Error(errorText(error));
    }
  };

  useLeaveGuard(count > 0, (retry) => {
    leave.current = retry;
    setGuard((n) => n + 1);
  });

  const reloadButton = useRef<HTMLButtonElement>(null);
  const commands = useMemo<ScreenCommand[]>(() => [
    { id: "reload", label: "Reload saved settings", run: () => void reload(reloadButton.current) },
  ], [count]);
  useScreenCommands(commands);

  const setWorker = (key: string, patch: Partial<Worker>) => setWorkers((list) => list.map((w) => (w.key === key ? { ...w, ...patch } : w)));
  const setPool = (key: string, patch: Partial<Pool>) => setPools((list) => list.map((p) => (p.key === key ? { ...p, ...patch } : p)));

  // Removing asks first when the entry has a name, then moves focus to the next entry, the previous one, or the add button.
  const removeEntry = async <T extends { key: string }>(list: T[], entry: T, name: string, question: string, set: (next: T[]) => void,
    prefix: string, fallback: string) => {
    if (name && !await confirm({ title: t(question, { name }), confirmLabel: t("Remove"), danger: true, returnFocus: document.querySelector<HTMLElement>(fallback) })) return;
    const i = list.findIndex((e) => e.key === entry.key);
    const next = list[i + 1] ?? list[i - 1];
    set(list.filter((e) => e.key !== entry.key));
    focusLater(next ? `[data-entry="${prefix + next.key}"] input:not([readonly])` : fallback);
  };

  const images = (settings?.runner_images ?? []).map((entry) => entry.label).filter(Boolean);
  const docker = settings?.backend === "docker";

  return (
    <Page title={t("Settings")} description={t("Changes apply without restarting the manager.")}
      actions={<Button ref={reloadButton} variant="secondary" onClick={(event) => void reload(event.currentTarget)}>{t("Reload saved settings")}</Button>}>
      {loadError && (
        <Notice tone="danger" role="alert">
          <p>{loadError.message}</p>
          <Button variant="secondary" size="sm" onClick={() => void load()}>{t("Try again")}</Button>
        </Notice>
      )}
      {!settings && loading && <Skeleton />}
      {settings && saved && (
        <form className="settings-form" aria-label={t("Operational settings")} onSubmit={(event) => event.preventDefault()}
          onInvalidCapture={(event) => {
            // A closed pool cannot show or focus its invalid field: open it, then focus the first such field once it shows.
            const target = event.target as HTMLElement;
            const key = target.closest<HTMLElement>("[data-pool]")?.dataset.pool;
            if (!key || open.includes(key)) return;
            setOpen((keys) => (keys.includes(key) ? keys : [...keys, key]));
            if (focusing.current || target !== event.currentTarget.querySelector("input:invalid, textarea:invalid")) return;
            focusing.current = true;
            const focus = (tries: number) => requestAnimationFrame(() => {
              if (target.checkVisibility() || !tries) {
                focusing.current = false;
                target.focus();
              } else focus(tries - 1);
            });
            focus(30);
          }}>
          <FormSection title={t("Fleet")}>
            <Field label={t("Fleet slot limit")} required>
              <Input type="number" name="slot_limit" inputMode="numeric" min={1} required value={fleet.values.slot_limit}
                onChange={(event) => fleet.set("slot_limit", event.currentTarget.value)} />
            </Field>
            <Field label={t("GitHub polling interval")} required description={t(settings.webhook_enabled
              ? "GitHub webhooks are enabled. Set polling to 0 for webhook-only discovery."
              : "GitHub webhooks are disabled. Keep polling enabled to receive jobs.")}>
              <Input name="poll_interval" placeholder="30s" maxLength={32} required value={fleet.values.poll_interval}
                onChange={(event) => fleet.set("poll_interval", event.currentTarget.value)} />
            </Field>
            <Field label={t("Default runner image")} required>
              <Input name="runner_image" className="font-mono" maxLength={256} required spellCheck={false} value={fleet.values.runner_image}
                onChange={(event) => fleet.set("runner_image", event.currentTarget.value)} />
            </Field>
          </FormSection>

          {docker && (
            <FormSection title={t("Docker workers")} description={t("SSH workers use the manager's configured SSH credentials. Outbound agents use deployment credentials.")}>
              <ul className="entry-list">
                {workers.map((worker) => {
                  // Outbound agents enroll with deployment credentials, not from the panel.
                  const agent = worker.address.startsWith("agent://") && saved.workers.some((w) => w.key === worker.key);
                  const name = worker.address || t("New worker");
                  return (
                    <li key={worker.key} className="worker-row" data-entry={"w-" + worker.key}>
                      <Field label={t("Worker address")} className="toolbar-grow" description={agent ? t("Read-only: set in the deployment.") : undefined}>
                        <Input className="font-mono" placeholder="local or ssh://user@host" maxLength={260} required readOnly={agent}
                          spellCheck={false} value={worker.address} onChange={(event) => setWorker(worker.key, { address: event.currentTarget.value })} />
                      </Field>
                      <Field label={t("Worker slot limit")}>
                        <Input type="number" min={1} placeholder={t("Use fleet limit")} value={worker.limit}
                          onChange={(event) => setWorker(worker.key, { limit: event.currentTarget.value })} />
                      </Field>
                      {!agent && (
                        <Button type="button" variant="secondary" size="sm" aria-label={t("Remove worker {name}", { name })}
                          onClick={() => void removeEntry(workers, worker, worker.address.trim(),
                            "Remove the worker {name} from these settings? The change applies when you save.", setWorkers, "w-", "#add-worker")}>
                          {t("Remove")}
                        </Button>
                      )}
                    </li>
                  );
                })}
              </ul>
              <div>
                <Button id="add-worker" type="button" variant="secondary" size="sm" onClick={() => {
                  const worker = { key: newKey(), address: "", limit: "" };
                  setWorkers((list) => [...list, worker]);
                  focusLater(`[data-entry="w-${worker.key}"] input`);
                }}>{t("Add worker")}</Button>
              </div>
            </FormSection>
          )}

          {docker && (
            <FormSection title={t("Worker pools")} description={images.length
              ? t("Runner OS names jobs can select with os__NAME__: {names}", { names: images.join(", ") }) : undefined}>
              <Accordion multiple value={open} onValueChange={setOpen} className="pool-list">
                {pools.map((pool) => (
                  <Accordion.Item key={pool.key} value={pool.key} data-pool={pool.key} data-entry={"p-" + pool.key}>
                    <Accordion.Trigger level={3}>{pool.name || t("New pool")}</Accordion.Trigger>
                    <Accordion.Content>
                      <Fieldset legend={<span className="sr-only">{pool.name || t("New pool")}</span>} className="pool-grid">
                        <Field label={t("Pool name")} required>
                          <Input className="font-mono" pattern="[a-z0-9][a-z0-9\-]{0,31}" required spellCheck={false} value={pool.name}
                            onChange={(event) => setPool(pool.key, { name: event.currentTarget.value })} />
                        </Field>
                        <Field label={t("CPU limit")}>
                          <Input type="number" min={0.01} step="any" value={pool.cpus} onChange={(event) => setPool(pool.key, { cpus: event.currentTarget.value })} />
                        </Field>
                        <Field label={t("Memory limit (MiB)")}>
                          <Input type="number" min={6} value={pool.memory} onChange={(event) => setPool(pool.key, { memory: event.currentTarget.value })} />
                        </Field>
                        {/* The server refuses warm runners with a build cache or on outbound workers; its message shows on save. */}
                        <Field label={t("Warm runners per host")}>
                          <Input type="number" min={0} step={1} placeholder="0" value={pool.warm} onChange={(event) => setPool(pool.key, { warm: event.currentTarget.value })} />
                        </Field>
                        <Field label={t("Replace idle warm runners after (minutes)")}>
                          <Input type="number" min={0} step={1} placeholder="30" value={pool.warmIdle}
                            onChange={(event) => setPool(pool.key, { warmIdle: event.currentTarget.value })} />
                        </Field>
                        <Field label={t("Pool runner image")} className="span-all">
                          <Input className="font-mono" maxLength={256} placeholder={t("Use default image")} spellCheck={false} value={pool.image}
                            onChange={(event) => setPool(pool.key, { image: event.currentTarget.value })} />
                        </Field>
                        <Field label={t("Allowed runner OS")} className="span-all"
                          description={t("Comma-separated runner image catalogue names, such as ubuntu22.04, debian12")}>
                          <Input className="font-mono" maxLength={2700} placeholder={t("Allow every runner OS")} spellCheck={false} value={pool.images}
                            onChange={(event) => setPool(pool.key, { images: event.currentTarget.value })} />
                        </Field>
                        <Field label={t("Worker addresses")} className="span-half">
                          <Textarea className="font-mono" minRows={2} maxRows={8} placeholder={t("One worker address per line")} spellCheck={false}
                            value={pool.hosts} onChange={(event) => setPool(pool.key, { hosts: event.currentTarget.value })} />
                        </Field>
                        <Field label={t("Allowed repositories")} className="span-half" disabled={!pool.restrict}>
                          <Textarea className="font-mono" minRows={2} maxRows={8} placeholder={t("One OWNER/REPO per line")} spellCheck={false}
                            value={pool.repositories} onChange={(event) => setPool(pool.key, { repositories: event.currentTarget.value })} />
                        </Field>
                        <p className="span-all text-sm text-text-hint">
                          {t("Warm runners start before a job arrives, on local and SSH hosts only, and cannot be combined with a shared build cache.")}
                        </p>
                        <div className="span-all pool-foot">
                          <Checkbox label={t("Run Docker-in-Docker (privileged)")} checked={pool.privileged}
                            onCheckedChange={(privileged) => setPool(pool.key, { privileged })} />
                          <Checkbox label={t("Keep a shared build cache")} checked={pool.cache} onCheckedChange={(cache) => setPool(pool.key, { cache })} />
                          <Checkbox label={t("Restrict repositories")} checked={pool.restrict} onCheckedChange={(restrict) => setPool(pool.key, { restrict })} />
                          <Button type="button" variant="secondary" size="sm" onClick={() => void removeEntry(pools, pool, pool.name.trim(),
                            "Remove the pool {name} from these settings? The change applies when you save.", setPools, "p-", "#add-pool")}>
                            {t("Remove pool")}
                          </Button>
                        </div>
                      </Fieldset>
                    </Accordion.Content>
                  </Accordion.Item>
                ))}
              </Accordion>
              <div>
                <Button id="add-pool" type="button" variant="secondary" size="sm" onClick={() => {
                  const pool = blankPool();
                  setPools((list) => [...list, pool]);
                  setOpen((keys) => [...keys, pool.key]);
                  focusLater(`[data-entry="p-${pool.key}"] input`);
                }}>{t("Add pool")}</Button>
              </div>
            </FormSection>
          )}

          <SaveBar count={count} onSave={save} onRevert={revert} guard={guard} messages={labels.saveBar}
            onDiscard={() => {
              revert();
              leave.current?.();
            }} />
        </form>
      )}
      <p className="text-sm text-text-hint">{fill("Queue backups are on the {link} screen.", { link: <a href="#/backups">{t("Backups")}</a> })}</p>
    </Page>
  );
}
