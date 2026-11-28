// G2 BarChart: one column per UTC day (v1 usage.js chart). The figure is one image with a summary name; each column
// has a <title>, and the table under it carries every number.
import { day } from "../format";
import { t } from "../i18n";

export function BarChart<T extends { key: string }>({ rows, value, text, metric }: {
  rows: T[]; value: (row: T) => number; text: (row: T) => string; /** Translated metric name. */ metric: string;
}) {
  const values = rows.map(value);
  const top = Math.max(1, ...values);
  const peak = rows[values.indexOf(top)];
  const width = Math.max(1, rows.length) * 10;
  const height = 100;
  return (
    <figure className="bar-chart">
      <div className="bar-chart-scale text-xs text-text-hint" aria-hidden>
        <span>{peak && values.some(Boolean) ? text(peak) : "0"}</span><span>0</span>
      </div>
      <svg viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" role="img"
        aria-label={t("{metric} per day, highest {value}", { metric, value: peak ? text(peak) : "0" })}>
        <line className="bar-chart-axis" x1={0} y1={height - 0.5} x2={width} y2={height - 0.5} />
        {rows.map((row, i) => {
          const bar = Math.max(values[i] ? 1.5 : 0, (values[i] / top) * (height - 4));
          return (
            <rect key={row.key} x={i * 10 + 1.5} y={height - bar} width={7} height={bar}>
              <title>{day(row.key, true) + ": " + text(row)}</title>
            </rect>
          );
        })}
      </svg>
      <figcaption className="bar-chart-caption text-xs text-text-hint">
        <span>{rows.length ? day(rows[0].key, true) : ""}</span>
        <span>{t("{metric} per day (UTC)", { metric })}</span>
        <span>{rows.length ? day(rows[rows.length - 1].key, true) : ""}</span>
      </figcaption>
    </figure>
  );
}
