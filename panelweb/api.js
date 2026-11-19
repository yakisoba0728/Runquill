// Panel API client. Requests are same-origin fetches, so the browser sends the
// session cookie and Origin header the server checks; nothing else is added.
// Errors carry the HTTP status and a message already passed through serverText.
import { serverText } from "./i18n.js";

let unauthorized = () => {};

// app.js registers the sign-in screen here; any 401 ends the session.
export function onUnauthorized(handler) {
  unauthorized = handler;
}

// ApiError is every failed request. raw is the response body as sent. Most
// handlers answer with a plain-text sentence; some (Migrate, runner logs)
// send JSON {error, code, pull_request}, whose parts become .code and
// .pullRequest. status is 0 when no response arrived.
export class ApiError extends Error {
  constructor(status, raw, { code = "", pullRequest = "" } = {}) {
    let text = String(raw ?? "").trim();
    if (text.startsWith("{")) {
      try {
        const body = JSON.parse(text);
        if (body && typeof body.error === "string") {
          text = body.error;
          code ||= typeof body.code === "string" ? body.code : "";
          pullRequest ||= typeof body.pull_request === "string" ? body.pull_request : "";
        }
      } catch {}
    }
    super(serverText(text) || serverText("Request failed."));
    this.status = status;
    this.raw = raw;
    this.code = code;
    this.pullRequest = pullRequest;
  }
}

async function failure(response) {
  const error = new ApiError(response.status, await response.text().catch(() => ""));
  if (response.status === 401) unauthorized();
  return error;
}

// unreachable turns a network failure or timeout into an ApiError, and lets an
// abort the caller asked for through unchanged.
function unreachable(error, signal) {
  if (signal?.aborted && error?.name === "AbortError") return error;
  return new ApiError(0, error?.name === "TimeoutError" ? "The manager did not answer in time. Try again." : "Could not reach the manager. Check the connection.");
}

// request sends JSON when data is given and resolves to the decoded body, or
// null for 204. fetch is looked up per call so tests can stub it.
export async function request(path, { method = "GET", data, timeout = 7000, signal } = {}) {
  const deadline = AbortSignal.timeout(timeout);
  const combined = signal ? AbortSignal.any([signal, deadline]) : deadline;
  let response;
  try {
    response = await globalThis.fetch(path, {
      method, cache: "no-store", signal: combined,
      headers: data === undefined ? undefined : { "Content-Type": "application/json" },
      body: data === undefined ? undefined : JSON.stringify(data),
    });
  } catch (error) {
    throw unreachable(error, signal);
  }
  if (!response.ok) throw await failure(response);
  return response.status === 204 ? null : response.json();
}

// download POSTs to path without a deadline and reads the body in chunks,
// calling progress(received, total) as it arrives. Resolves to a Blob.
export async function download(path, type, progress = () => {}) {
  let response;
  try {
    response = await globalThis.fetch(path, { method: "POST", cache: "no-store" });
  } catch (error) {
    throw unreachable(error);
  }
  if (!response.ok) throw await failure(response);
  const total = Number(response.headers.get("Content-Length")) || 0;
  const chunks = [];
  let received = 0;
  const reader = response.body.getReader();
  for (let part = await reader.read(); !part.done; part = await reader.read()) {
    chunks.push(part.value);
    received += part.value.byteLength;
    progress(received, total);
  }
  return new Blob(chunks, { type });
}

// stream GETs path without a deadline and resolves to the Response once its
// headers arrive, for bodies read as they come (runner logs). Failures are
// ApiErrors like any request, so a 401 still ends the session; aborting
// signal rejects with the AbortError.
export async function stream(path, { signal } = {}) {
  let response;
  try {
    response = await globalThis.fetch(path, { cache: "no-store", signal });
  } catch (error) {
    throw unreachable(error, signal);
  }
  if (!response.ok) throw await failure(response);
  return response;
}

// createApi gives each view the same interface in both modes. In the sample
// panel, requests are answered by that view's fixtures (demo/<id>.js) and
// every change is refused, so nothing leaves the browser.
export function createApi({ demo = false, fixtures = null } = {}) {
  if (!demo) {
    return {
      get: (path, options) => request(path, options),
      post: (path, data, options) => request(path, { ...options, method: "POST", data }),
      put: (path, data, options) => request(path, { ...options, method: "PUT", data }),
      del: (path, options) => request(path, { ...options, method: "DELETE" }),
      download,
      stream,
    };
  }
  const refused = () => Promise.reject(new ApiError(0, "Not available in the sample panel."));
  const answer = async (method, path, data) => {
    const url = new URL(path, location.href);
    const route = fixtures?.[method + " " + url.pathname.replace(/^.*?(\/api\/)/, "/api/")];
    if (method !== "GET" || !route) throw new ApiError(0, "Not available in the sample panel.");
    return structuredClone(await route(url.searchParams, data));
  };
  return {
    get: (path) => answer("GET", path),
    post: (path, data) => answer("POST", path, data),
    put: (path, data) => answer("PUT", path, data),
    del: (path) => answer("DELETE", path),
    download: refused,
    stream: refused,
  };
}
