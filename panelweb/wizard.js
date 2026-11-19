// Loaded without defer from <head>: the theme is applied before the first
// paint, then the rest waits for the document. The appearance choice uses the
// same storage key and values as the panel's theme.js and app.js.
(() => {
  let choice = "system";
  try {
    choice = localStorage.getItem("runquill-theme") || localStorage.getItem("runquill-login-theme") || "system";
  } catch {}
  document.documentElement.dataset.theme = choice === "light" || choice === "dark" ? choice : "system";
})();

document.addEventListener("DOMContentLoaded", () => {
  for (const control of document.querySelectorAll("[data-theme-control]")) {
    control.value = document.documentElement.dataset.theme || "system";
    control.addEventListener("change", () => {
      document.documentElement.dataset.theme = control.value;
      try { localStorage.setItem("runquill-theme", control.value); } catch {}
    });
    const field = control.closest("[data-theme-field]");
    if (field) field.hidden = false;
  }

  // Copy buttons for commands and addresses. They stay hidden without this script.
  const copyStatus = document.getElementById("copy-status");
  for (const button of document.querySelectorAll(".value > .copy")) {
    const code = button.parentElement.querySelector("code");
    if (!code || !copyStatus) continue;
    const label = copyStatus.dataset.copy || "Copy";
    button.textContent = label;
    button.setAttribute("aria-label", `${label}: ${code.textContent}`);
    button.hidden = false;
    let reset = 0;
    button.addEventListener("click", async () => {
      let message = copyStatus.dataset.copied || "Copied";
      try {
        await navigator.clipboard.writeText(code.textContent);
        button.dataset.done = "";
        button.textContent = message;
      } catch {
        // Without clipboard access (for example over plain HTTP on another host), select the text instead.
        const range = document.createRange();
        range.selectNodeContents(code);
        const selection = getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
        message = copyStatus.dataset.manual || "Selected";
      }
      copyStatus.textContent = message;
      clearTimeout(reset);
      reset = setTimeout(() => {
        delete button.dataset.done;
        button.textContent = label;
        copyStatus.textContent = "";
      }, 2000);
    });
  }

  // Enter on a scope option must not submit before the user activates Connect GitHub.
  document.querySelector("[data-repository-scope]")?.addEventListener("keydown", (event) => {
    if (event.key === "Enter") event.preventDefault();
  });

  const backend = document.querySelector("[name=backend]");
  if (backend) {
    const k3sOnly = document.querySelectorAll("[data-k3s]");
    const dockerOnly = document.querySelectorAll("[data-docker]");
    const contextInput = document.querySelector("[name=context]");
    const blocked = document.querySelector("[data-docker-blocked]");
    const start = document.querySelector("[data-start]");
    const updateBackend = () => {
      const k3s = backend.value === "k3s";
      for (const element of k3sOnly) element.hidden = !k3s;
      for (const element of dockerOnly) element.hidden = k3s;
      contextInput.required = k3s;
      // Docker that this user cannot reach would fail at deployment; the server refuses it too.
      if (start) {
        if (blocked && !k3s) start.setAttribute("aria-disabled", "true");
        else start.removeAttribute("aria-disabled");
      }
    };
    backend.addEventListener("change", updateBackend);
    updateBackend();
    start?.form.addEventListener("submit", (event) => {
      if (start.getAttribute("aria-disabled") !== "true") return;
      event.preventDefault();
      blocked.setAttribute("tabindex", "-1");
      blocked.focus();
    });
    for (const field of document.querySelectorAll("[aria-invalid=true]")) {
      const clear = () => {
        field.removeAttribute("aria-invalid");
        // Keep the field's own help text described after the error is dismissed.
        const rest = (field.getAttribute("aria-describedby") || "").split(" ").filter((id) => id && id !== "wizard-error");
        if (rest.length) field.setAttribute("aria-describedby", rest.join(" "));
        else field.removeAttribute("aria-describedby");
      };
      field.addEventListener("input", clear, { once: true });
      field.addEventListener("change", clear, { once: true });
    }
  }

  const { phase, step, install } = document.body.dataset;
  const activity = document.getElementById("wizard-activity");
  const log = document.getElementById("wizard-log");
  const connection = document.getElementById("wizard-connection");
  const pollDelay = 1000;
  const maxPollDelay = 10000;
  let delay = pollDelay;
  let timer = 0;
  let inFlight = false;
  let stopped = false;

  function updateLog(text) {
    if (!log || log.textContent === text) return;
    const atEnd = log.scrollHeight - log.scrollTop - log.clientHeight < 24;
    log.textContent = text;
    if (atEnd) log.scrollTop = log.scrollHeight;
  }

  function showConnection(key) {
    if (!connection) return;
    const text = key ? connection.dataset[key] || "" : "";
    connection.dataset.state = key === "ended" ? "failed" : "waiting";
    if (connection.textContent !== text) connection.textContent = text;
  }

  async function poll() {
    try {
      const response = await fetch("/status", { cache: "no-store", signal: AbortSignal.timeout(5000) });
      if (response.status === 401) return "ended";
      if (!response.ok) return "failed";
      const status = await response.json();
      if (status.phase !== phase || status.step !== step || String(status.install) !== install) {
        location.reload();
        return "reload";
      }
      const sentence = activity?.querySelector("[data-activity]");
      if (sentence && sentence.textContent !== status.activity) sentence.textContent = status.activity;
      updateLog(status.log);
      return "ok";
    } catch {
      // Keep the last visible output while the wizard is unreachable.
      return "failed";
    }
  }

  function schedule() {
    clearTimeout(timer);
    timer = stopped || document.hidden ? 0 : setTimeout(refresh, delay);
  }

  async function refresh() {
    if (inFlight || stopped) return;
    inFlight = true;
    const result = await poll();
    inFlight = false;
    if (result === "ended" || result === "reload") {
      stopped = true;
      showConnection(result === "ended" ? "ended" : "");
      return;
    }
    // Back off while the wizard is unreachable, and resume the normal pace once it answers.
    showConnection(result === "failed" ? "reconnecting" : "");
    delay = result === "ok" ? pollDelay : Math.min(delay * 2, maxPollDelay);
    schedule();
  }

  if (log) log.scrollTop = log.scrollHeight;
  if (phase === "working" || phase === "deploying") {
    document.addEventListener("visibilitychange", () => {
      if (stopped) return;
      clearTimeout(timer);
      timer = 0;
      if (!document.hidden) void refresh();
    });
    void refresh();
  }
});
