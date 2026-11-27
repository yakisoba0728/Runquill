// G9: a read-only value with a copy button (v1 copyButton). Without clipboard access it selects the text for a manual
// copy. The result goes to report, an inline status line, because the field sits in a modal dialog.
import { Button, Field, Input } from "@yakihyuk0728/stratum-ui";
import { useRef } from "react";
import { t } from "../i18n";

export function CopyField({ label, value, copyLabel, report, autoFocus }: {
  label: string; value: string; copyLabel: string; report: (text: string) => void; autoFocus?: boolean;
}) {
  const input = useRef<HTMLInputElement>(null);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      report(t("Command copied."));
    } catch {
      input.current?.focus();
      input.current?.select();
      report(t("Copy the selected text."));
    }
  };
  return (
    <div className="copy-field">
      <Field label={label} className="toolbar-grow">
        <Input ref={input} readOnly value={value} className="font-mono" data-autofocus={autoFocus || undefined}
          onFocus={(event) => event.currentTarget.select()} spellCheck={false} />
      </Field>
      <Button type="button" variant="secondary" onClick={() => void copy()}>{copyLabel}</Button>
    </div>
  );
}
