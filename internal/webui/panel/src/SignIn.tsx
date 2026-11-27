// The sign-in screen (design §3.1, §4.1). "Continue with GitHub" is a link: the panel CSP has form-action 'none'.
import { SegmentedControl } from "@yakihyuk0728/stratum-ui";
import { useEffect, useRef } from "react";
import { Notice } from "./components";
import { language, t } from "./i18n";
import { languageURL, setTheme, useTheme, type Theme } from "./theme";

// A refused GitHub sign-in returns with a fixed reason code (panel_access.go signInFailed).
const reasons: Record<string, string> = {
  expired: "Your sign-in expired. Continue with GitHub to try again.",
  cancelled: "Sign-in was cancelled or already used. Continue with GitHub to try again.",
  github: "Could not verify the GitHub login. Try again.",
  not_enrolled: "Your GitHub account is not enrolled in this Runquill panel.",
  unavailable: "Could not start a panel session.",
};

/** The ?signin= reason as an English key ("" when absent or unknown), dropped from the address so it shows once. */
export function takeSignInReason() {
  const url = new URL(location.href);
  const code = url.searchParams.get("signin");
  if (code == null) return "";
  url.searchParams.delete("signin");
  history.replaceState(history.state, "", url);
  return Object.hasOwn(reasons, code) ? reasons[code] : "";
}

export function SignIn({ reason, focus }: { reason?: string; focus?: boolean }) {
  const theme = useTheme();
  const heading = useRef<HTMLHeadingElement>(null);
  const other = language === "ko" ? "en" : "ko";
  useEffect(() => {
    document.title = t("Sign in to Runquill");
    if (focus) heading.current?.focus();
  }, [focus, reason]);
  return (
    <main className="signin">
      <div className="signin-box">
        <p className="brand">runquill</p>
        <h1 ref={heading} tabIndex={-1}>{t("Sign in to Runquill")}</h1>
        <p className="text-text-hint">{t("Use the GitHub account your Runquill administrator enrolled.")}</p>
        {reason && <Notice tone="danger" role="alert">{t(reason)}</Notice>}
        <a className="btn btn-block" href="./auth/github">{t("Continue with GitHub")}</a>
        <dl className="kv">
          <div><dt>{t("Panel address")}</dt><dd className="font-mono">{location.host}</dd></div>
        </dl>
        <div className="signin-controls">
          <a href={languageURL(other)} lang={other}>{other === "en" ? "English" : "한국어"}</a>
          <SegmentedControl
            size="sm"
            aria-label={t("Appearance")}
            value={theme}
            onValueChange={(value) => setTheme(value as Theme)}
            options={[{ value: "system", label: t("System") }, { value: "light", label: t("Light") }, { value: "dark", label: t("Dark") }]}
          />
        </div>
      </div>
    </main>
  );
}
