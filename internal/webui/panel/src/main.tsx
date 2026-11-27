// Entry: language (Korean loads its catalogue first) → session (or the sample panel) → render.
// index.html carries data-mode="demo"; the manager serves it as data-mode="live".
import "@yakihyuk0728/stratum-ui/stratum.css";
import "./panel.css";
import { createRoot } from "react-dom/client";
import { onUnauthorized, request, type ApiError } from "./api";
import { preferredLanguage, setLanguage } from "./i18n";
import { live } from "./live";
import { Shell } from "./Shell";
import { SignIn, takeSignInReason } from "./SignIn";
import type { User } from "./types";

const root = createRoot(document.getElementById("root")!);

function signedOut(reason: string, focus: boolean) {
  live.stop();
  root.render(<SignIn reason={reason} focus={focus} />);
}

async function start() {
  await setLanguage(preferredLanguage());
  if (document.body.dataset.mode === "demo") {
    const { sample } = await import("./demo");
    live.sample(sample("first").snapshot);
    root.render(<Shell user={null} sample={sample} />);
    return;
  }
  const reason = takeSignInReason();
  let user: User;
  try {
    user = await request<User>("./api/me");
  } catch (error) {
    signedOut((error as ApiError).status === 401 ? reason : "The panel could not check your session. Try again later.", false);
    return;
  }
  // Any later 401 (a request or /snapshot) ends the session: the shell unmounts, closing open dialogs.
  onUnauthorized(() => signedOut("Your session ended. Sign in again to view the fleet.", true));
  live.start();
  root.render(<Shell user={user} />);
}

void start();
