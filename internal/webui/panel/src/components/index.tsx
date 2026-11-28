// Small local components for what Stratum 0.1.1 lacks (design §8). Each is a stand-in until Stratum has its own.
import { Badge, Button, PageHeader } from "@yakihyuk0728/stratum-ui";
import { Component, useEffect, useLayoutEffect, useRef, type ComponentProps, type ReactNode } from "react";
import { relative, useNow } from "../format";
import { statusLabel, statusTone } from "../fleet";
import { language, t } from "../i18n";
import { takeFocus, useVisit } from "../router";

/** G6: a band that stays (lost connection, server errors, sign-in reasons). Stratum Callout later. */
export function Notice({ tone = "warning", className = "", children, ...rest }: ComponentProps<"div"> & { tone?: "info" | "warning" | "danger" }) {
  return <div className={"notice " + className} data-tone={tone} {...rest}>{children}</div>;
}

/** A polite status line for an action's progress and result (v1 statusLine); an empty one stays in place, silent. */
export function StatusLine({ text, tone }: { text?: ReactNode; tone?: "ok" | "failed" }) {
  return <p role="status" className="status-line" data-tone={tone}>{text}</p>;
}

/** G5: a <dl> of label and value rows. Stratum DescriptionList later. */
export function KeyValue({ rows, label }: { rows: [ReactNode, ReactNode][]; label?: string }) {
  return (
    <dl className="kv" aria-label={label}>
      {rows.map(([key, value], i) => <div key={i}><dt>{key}</dt><dd>{value}</dd></div>)}
    </dl>
  );
}

/** G1: a capacity meter on the native <meter>. Stratum Meter later. */
export function Meter({ value, max, label }: { value: number; max: number; label: string }) {
  return <meter className="meter" min={0} max={Math.max(1, max)} value={value} high={max * 0.85} optimum={0} aria-label={label} />;
}

/** G12: a number tile. */
export function StatTile({ label, value, children }: { label: ReactNode; value: ReactNode; children?: ReactNode }) {
  return (
    <div className="stat rounded-lg border border-line p-4">
      <div className="text-sm text-text-hint">{label}</div>
      <div className="stat-value tabular-nums">{value}</div>
      {children}
    </div>
  );
}

/** G10: a numbered list of steps with a done marker. */
export function Steps({ steps }: { steps: { title: ReactNode; done: boolean; note: ReactNode }[] }) {
  return (
    <ol className="steps">
      {steps.map((step, i) => (
        <li key={i} data-done={step.done || undefined}>
          <Badge tone={step.done ? "success" : "neutral"} dot>{t(step.done ? "Done" : "Not yet")}</Badge>
          <h3>{step.title}</h3>
          <p>{step.note}</p>
        </li>
      ))}
    </ol>
  );
}

/** A status code as a coloured badge; label replaces the code's own name (already translated). */
export function StatusBadge({ status, label }: { status: string; label?: string }) {
  return <Badge tone={statusTone(status)} dot>{label ?? statusLabel(status)}</Badge>;
}

/** G16: "3s ago", ticking on the shared clock. */
export function Ago({ at }: { at?: string | null }) {
  const now = useNow();
  return <span title={at ?? undefined}>{relative(at, now)}</span>;
}

/** "Updated 3s ago" (lower case after "Updated" in English, as in v1). */
export function UpdatedAgo({ at }: { at: string }) {
  const ago = relative(at, useNow());
  return <>{t("Updated {time}", { time: language === "ko" ? ago : ago.toLowerCase() })}</>;
}

/** Every screen's header: sets the document title and takes focus after a navigation (not on first load). */
export function Page({ title, description, actions, tabs, children }: {
  title: string; description?: ReactNode; actions?: ReactNode; tabs?: ReactNode; children?: ReactNode;
}) {
  const visit = useVisit();
  const header = useRef<HTMLElement>(null);
  useEffect(() => {
    document.title = title + " · Runquill";
  }, [title]);
  useLayoutEffect(() => {
    if (!takeFocus()) return;
    const heading = header.current?.querySelector<HTMLElement>(".page-title");
    if (!heading) return;
    heading.tabIndex = -1;
    heading.focus({ preventScroll: true });
    heading.closest(".shell-main")?.scrollTo(0, 0);
  }, [visit]);
  return (
    <>
      <PageHeader ref={header} title={title} description={description} actions={actions} tabs={tabs} />
      <div className="page-content">{children}</div>
    </>
  );
}

/** A screen chunk that fails to load (often an upgraded manager renamed it) or throws while rendering. */
export class ScreenBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  componentDidCatch(error: unknown) {
    console.error(error);
  }
  render() {
    if (!this.state.failed) return this.props.children;
    return (
      <Notice tone="danger" role="alert" className="m-6">
        <p>{t("This screen could not be loaded. The manager may have been updated.")}</p>
        <Button variant="secondary" size="sm" onClick={() => location.reload()}>{t("Reload")}</Button>
      </Notice>
    );
  }
}
