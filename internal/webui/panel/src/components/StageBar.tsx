// G2 StageBar: a job's observed stages as one stacked bar over a legend that carries every number, so the bar itself
// is decorative (v1 jobinsight.js timingView).
import { stages } from "../fleet";
import { duration } from "../format";
import { t } from "../i18n";
import type { JobTiming } from "../types";

export function StageBar({ timing }: { timing?: JobTiming }) {
  const list = stages(timing);
  const shown = list.filter((stage) => stage.seconds !== null && stage.seconds > 0);
  const total = shown.reduce((sum, stage) => sum + stage.seconds!, 0);
  // Execution can dwarf the millisecond stages, so each segment keeps a minimum width.
  const shares = shown.map((stage) => Math.max(0.025, stage.seconds! / total));
  const sum = shares.reduce((a, b) => a + b, 0);
  let x = 0;
  return (
    <div className="stage-timing">
      {shown.length > 0 && (
        <>
          <svg className="stage-bar" viewBox="0 0 100 10" preserveAspectRatio="none" aria-hidden focusable="false">
            {shown.map((stage, i) => {
              const width = (shares[i] / sum) * 100;
              const rect = <rect key={stage.key} x={x} y={0} width={Math.max(0.2, width - 0.4)} height={10} data-stage={stage.key} />;
              x += width;
              return rect;
            })}
          </svg>
          <p className="text-sm text-text-hint">{t("{time} across the observed stages", { time: duration(total) })}</p>
        </>
      )}
      <dl className="stage-legend" aria-label={t("Stage timing")}>
        {list.map((stage) => (
          <div key={stage.key} data-unobserved={stage.seconds === null || undefined}>
            <dt><span className="stage-swatch" data-stage={stage.key} />{t(stage.label)}</dt>
            <dd className="tabular-nums">{stage.text}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
