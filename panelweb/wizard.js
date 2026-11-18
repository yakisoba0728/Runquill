const backend = document.querySelector("[name=backend]");
if (backend) {
  const contextLabel = document.querySelector("[data-k3s]");
  const updateContext = () => { contextLabel.hidden = backend.value !== "k3s"; };
  backend.addEventListener("change", updateContext);
  updateContext();
}

const { phase, step, install, stage } = document.body.dataset;
const activity = document.getElementById("wizard-activity");
const log = document.getElementById("wizard-log");
const korean = document.documentElement.lang === "ko";
const steps = {
  images: ["러너 이미지를 준비하고 있습니다.", "Preparing runner images."],
  resources: ["k3s 리소스를 적용하고 있습니다.", "Applying k3s resources."],
  manager: ["매니저를 시작하고 있습니다.", "Starting the manager."],
  network: ["Tailscale 연결을 적용하고 있습니다.", "Applying Tailscale settings."],
  check: ["서버와 GitHub 연결을 점검하고 있습니다.", "Checking the server and GitHub connection."],
};

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

async function refresh() {
  try {
    const response = await fetch("/status", { cache: "no-store", signal: AbortSignal.timeout(5000) });
    if (response.status === 401) {
      if (activity) activity.textContent = korean ? "설치 세션이 끝났습니다. 새 설치 링크를 열어 주세요." : "This setup session ended. Open a new setup link.";
      return;
    }
    if (response.ok) {
      const status = await response.json();
      if (status.phase !== phase || status.step !== step || String(status.install) !== install) {
        location.reload();
        return;
      }
      updateStage(status.stage);
      updateLog(status.log);
    }
  } catch {
    // Keep the last visible output while reconnecting to the wizard.
  }
  setTimeout(refresh, 1000);
}

updateStage(stage);
if (log) log.scrollTop = log.scrollHeight;
if (phase === "working" || phase === "deploying") void refresh();
