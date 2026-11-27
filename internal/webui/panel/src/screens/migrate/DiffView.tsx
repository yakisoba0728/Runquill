// G11: a preview's unified diff as a table of old and new line numbers (v1 migrate.js diffTable). The lines are
// repository content: text only. The scroller is a tab stop so the keyboard can scroll long lines sideways.
import { t } from "../../i18n";
import { diffLines } from "./mapping";

export function DiffView({ path, diff }: { path: string; diff: string }) {
  const label = t("Changes in {path}", { path });
  return (
    <div className="table-scroll" role="region" tabIndex={0} aria-label={label}>
      <table className="diff-view">
        <caption className="sr-only">{label}</caption>
        <thead>
          <tr>
            <th scope="col" className="num">{t("Old")}</th>
            <th scope="col" className="num">{t("New")}</th>
            <th scope="col"><span className="sr-only">{t("Change")}</span></th>
            <th scope="col">{t("Line")}</th>
          </tr>
        </thead>
        <tbody>
          {diffLines(diff).map((line, i) => {
            if (line.kind === "hunk") return <tr key={i} className="diff-hunk"><td colSpan={4}>{line.text}</td></tr>;
            if (line.kind === "note") return <tr key={i} className="diff-note"><td colSpan={3} /><td>{t("No newline at end of file")}</td></tr>;
            const marker = line.kind === "removed" ? "-" : line.kind === "added" ? "+" : " ";
            return (
              <tr key={i} data-change={line.kind === "same" ? undefined : line.kind}>
                <td className="num">{line.old ?? ""}</td>
                <td className="num">{line.new ?? ""}</td>
                <td className="diff-marker">
                  {marker}
                  {line.kind !== "same" && <span className="sr-only">{t(line.kind === "removed" ? "Removed" : "Added")}</span>}
                </td>
                <td className="diff-code">{line.text}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
