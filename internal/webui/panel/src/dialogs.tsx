// confirm(): one ConfirmDialog for the whole panel, asked as a promise (the v1 ctx.confirm contract).
import { ConfirmDialog } from "@yakihyuk0728/stratum-ui";
import { useEffect, useState, type ReactNode } from "react";
import { stratum } from "./i18n/stratum";

interface Question {
  title: ReactNode;
  description?: ReactNode;
  confirmLabel: ReactNode;
  danger?: boolean;
}

let ask: ((question: Question & { answer: (yes: boolean) => void }) => void) | null = null;

/** Resolves true when confirmed, false when cancelled or dismissed. */
export function confirm(question: Question): Promise<boolean> {
  return new Promise((answer) => (ask ? ask({ ...question, answer }) : answer(false)));
}

export function ConfirmHost() {
  const [question, setQuestion] = useState<(Question & { answer: (yes: boolean) => void }) | null>(null);
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
      }}
    />
  );
}
