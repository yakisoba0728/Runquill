// confirm(): one ConfirmDialog for the whole panel, asked as a promise (the v1 ctx.confirm contract).
import { ConfirmDialog } from "@yakihyuk0728/stratum-ui";
import { useEffect, useState, type ReactNode } from "react";
import { stratum } from "./i18n/stratum";

interface Question {
  title: ReactNode;
  description?: ReactNode;
  confirmLabel: ReactNode;
  danger?: boolean;
  /** Where focus goes after the dialog when the element that opened it is gone (the palette, for one). */
  returnFocus?: HTMLElement | null;
}

type Asked = Question & { answer: (yes: boolean) => void };
let ask: ((question: Asked) => void) | null = null;

/**
 * After a dialog closes: when focus went nowhere because the element that opened it is gone or in a closed dialog still
 * sliding out (the command palette), put it on fallback instead.
 */
export function refocus(fallback: HTMLElement | null | undefined) {
  requestAnimationFrame(() => {
    const active = document.activeElement;
    if (!active || active === document.body || active.closest("dialog:not([open])")) fallback?.focus();
  });
}

/** Resolves true when confirmed, false when cancelled or dismissed. */
export function confirm(question: Question): Promise<boolean> {
  return new Promise((answer) => (ask ? ask({ ...question, answer }) : answer(false)));
}

export function ConfirmHost() {
  const [question, setQuestion] = useState<Asked | null>(null);
  useEffect(() => {
    ask = setQuestion;
    return () => void (ask = null);
  }, []);
  if (!question) return null;
  return (
    <ConfirmDialog
      open
      title={question.title}
      description={question.description}
      confirmLabel={question.confirmLabel}
      cancelLabel={stratum().confirm.cancelLabel}
      tone={question.danger ? "danger" : "default"}
      onConfirm={() => question.answer(true)}
      onOpenChange={(open) => {
        if (open) return;
        question.answer(false); // no effect after a confirm: a promise settles once
        setQuestion(null);
        refocus(question.returnFocus);
      }}
    />
  );
}
