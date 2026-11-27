// "Check repository" for admins (v1 queue.js diagnoseBlock): POST /api/diagnose {repository} with a 10 s limit, then
// the server's report. Errors stay in the dialog, next to the form that caused them.
import { Button, Dialog, Field, Input } from "@yakihyuk0728/stratum-ui";
import { useState } from "react";
import { errorText, request, type ApiError } from "../../api";
import { Notice } from "../../components";
import { fill, t } from "../../i18n";

type Result = { ok: boolean; report: string } | { error: string } | null;

export function Diagnose({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const [repository, setRepository] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<Result>(null);
  const check = async () => {
    if (busy) return;
    setBusy(true);
    setResult(null);
    try {
      setResult(await request<{ ok: boolean; report: string }>("./api/diagnose", { method: "POST", data: { repository: repository.trim() }, timeout: 10000 }));
    } catch (error) {
      // A 401 has already ended the session.
      if ((error as ApiError).status !== 401) setResult({ error: errorText(error) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange} size="lg">
      <form onSubmit={(event) => {
        event.preventDefault();
        void check();
      }}>
        <Dialog.Body>
          <Dialog.Title>{t("Check repository")}</Dialog.Title>
          <Dialog.Description>
            {fill("Missing a job? Run {command} to check GitHub App access, its job label and delivery.", { command: <code>runquill diagnose OWNER/REPO</code> })}
          </Dialog.Description>
          <Field label={t("GitHub repository")} required>
            <Input data-autofocus value={repository} onChange={(event) => setRepository(event.currentTarget.value)} required maxLength={140}
              placeholder="OWNER/REPO" autoComplete="off" spellCheck={false} />
          </Field>
          {result && "error" in result && <Notice tone="danger" role="alert"><p>{result.error}</p></Notice>}
          {result && "report" in result && (
            <>
              <Notice tone={result.ok ? "info" : "warning"} role="status">
                <p>{t(result.ok ? "Repository check complete." : "Repository check needs attention.")}</p>
              </Notice>
              <pre className="report" tabIndex={0} role="region" aria-label={t("Report")}>{result.report}</pre>
            </>
          )}
        </Dialog.Body>
        <Dialog.Actions>
          <Button type="button" variant="secondary" onClick={() => onOpenChange(false)}>{t("Close")}</Button>
          <Button type="submit" loading={busy}>{t("Check repository")}</Button>
        </Dialog.Actions>
      </form>
    </Dialog>
  );
}
