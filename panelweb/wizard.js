const backend = document.querySelector("[name=backend]");
if (backend) {
  const contextLabel = document.querySelector("[data-k3s]");
  const contextInput = contextLabel.querySelector("input");
  const updateContext = () => {
    const k3s = backend.value === "k3s";
    contextLabel.hidden = !k3s;
    contextInput.required = k3s;
  };
  backend.addEventListener("change", updateContext);
  updateContext();
  for (const field of document.querySelectorAll("[aria-invalid=true]")) {
    const clear = () => {
      field.removeAttribute("aria-invalid");
      field.removeAttribute("aria-describedby");
    };
    field.addEventListener("input", clear, { once: true });
    field.addEventListener("change", clear, { once: true });
  }
}

const { phase, step, install, stage } = document.body.dataset;
const activity = document.getElementById("wizard-activity");
const log = document.getElementById("wizard-log");
const connection = document.getElementById("wizard-connection");
const korean = document.documentElement.lang === "ko";
const steps = {
  images: ["러너 이미지를 준비하고 있습니다.", "Preparing runner images."],
  resources: ["k3s 리소스를 적용하고 있습니다.", "Applying k3s resources."],
  manager: ["매니저를 시작하고 있습니다.", "Starting the manager."],
  network: ["Tailscale 연결을 적용하고 있습니다.", "Applying Tailscale settings."],
  check: ["서버와 GitHub 연결을 점검하고 있습니다.", "Checking the server and GitHub connection."],
};
const pollDelay = 1000;
const maxPollDelay = 10000;
let delay = pollDelay;
let timer = 0;
let inFlight = false;
let stopped = false;

function updateStage(next) {
  if (activity && Object.hasOwn(steps, next)) {
    const label = steps[next][korean ? 0 : 1];
    if (activity.textContent !== label) activity.textContent = label;
  }
}

function updateLog(text) {
  if (!log || log.textContent === text) return;
  const atEnd = log.scrollHeight - log.scrollTop - log.clientHeight < 24;
  log.textContent = text;
  if (atEnd) log.scrollTop = log.scrollHeight;
}

function showConnection(key) {
  if (!connection) return;
  const text = key ? connection.dataset[key] || "" : "";
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
    updateStage(status.stage);
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

updateStage(stage);
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
