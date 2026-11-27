// Panel API client, ported from panelweb/api.js. Same-origin fetches, so the browser sends the session cookie and the
// Origin header the server checks. Errors carry the HTTP status and a message already passed through serverText.
// In the sample panel, GETs are answered by the fixtures in src/demo (registered by main.tsx) and everything else is
// refused, so nothing leaves the browser (v1 createApi).
// shortcut: request and stream only; download (the backup POST with progress) arrives with the backups screen (phase 4).
import { serverText } from "./i18n";

let unauthorized = () => {};

/** main.tsx registers the sign-in screen here; any 401 ends the session. */
export function onUnauthorized(handler: () => void) {
  unauthorized = handler;
}

export function signalUnauthorized() {
  unauthorized();
}

/**
 * Every failed request. Most handlers answer with a plain-text sentence; some send JSON {error, code, pull_request}.
 * status is 0 when no response arrived.
 */
export class ApiError extends Error {
  status: number;
  raw: string;
  code = "";
  pullRequest = "";

  constructor(status: number, raw: string) {
    let text = String(raw ?? "").trim();
    let code = "";
    let pullRequest = "";
    if (text.startsWith("{")) {
      try {
        const body = JSON.parse(text);
        if (body && typeof body.error === "string") {
          text = body.error;
          code = typeof body.code === "string" ? body.code : "";
          pullRequest = typeof body.pull_request === "string" ? body.pull_request : "";
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

function unreachable(error: unknown, signal?: AbortSignal) {
  if (signal?.aborted && (error as Error)?.name === "AbortError") return error;
  return new ApiError(0, (error as Error)?.name === "TimeoutError"
    ? "The manager did not answer in time. Try again."
    : "Could not reach the manager. Check the connection.");
}

/** Answers a sample-panel GET: the path from /api/ on and its query; undefined when there is no fixture, null for 404. */
export type SampleAnswer = (path: string, query: URLSearchParams) => unknown;
let sample: SampleAnswer | null = null;

export function answerSamples(answer: SampleAnswer | null) {
  sample = answer;
}

/** The sample panel: no server, no writes, no polling. */
export const isSample = () => !!sample;

function sampleRequest<T>(path: string, method: string): Promise<T> {
  const url = new URL(path, location.href);
  const data = method === "GET" ? sample!(url.pathname.replace(/^.*?(\/api\/)/, "/api/"), url.searchParams) : undefined;
  if (data === undefined) return Promise.reject(new ApiError(0, "Not available in the sample panel."));
  if (data === null) return Promise.reject(new ApiError(404, "Not found."));
  return Promise.resolve(structuredClone(data) as T);
}

interface RequestOptions {
  method?: string;
  data?: unknown;
  timeout?: number;
  signal?: AbortSignal;
}

/** Sends JSON when data is given; resolves to the decoded body, or null for 204. */
export async function request<T>(path: string, { method = "GET", data, timeout = 7000, signal }: RequestOptions = {}): Promise<T> {
  if (sample) return sampleRequest<T>(path, method);
  const deadline = AbortSignal.timeout(timeout);
  let response: Response;
  try {
    response = await globalThis.fetch(path, {
      method,
      cache: "no-store",
      signal: signal ? AbortSignal.any([signal, deadline]) : deadline,
      headers: data === undefined ? undefined : { "Content-Type": "application/json" },
      body: data === undefined ? undefined : JSON.stringify(data),
    });
  } catch (error) {
    throw unreachable(error, signal);
  }
  if (!response.ok) {
    const error = new ApiError(response.status, await response.text().catch(() => ""));
    if (response.status === 401) unauthorized();
    throw error;
  }
  return (response.status === 204 ? null : await response.json()) as T;
}

/**
 * GETs path without a deadline and resolves to the Response once its headers arrive, for bodies read as they come
 * (runner logs). Failures are ApiErrors like any request, so a 401 still ends the session; aborting signal rejects with
 * the AbortError. The sample panel answers from its fixture as one finished body.
 */
export async function stream(path: string, { signal }: { signal?: AbortSignal } = {}): Promise<Response> {
  if (sample) return new Response(String(await sampleRequest<string>(path, "GET")));
  let response: Response;
  try {
    response = await globalThis.fetch(path, { cache: "no-store", signal });
  } catch (error) {
    throw unreachable(error, signal);
  }
  if (!response.ok) {
    const error = new ApiError(response.status, await response.text().catch(() => ""));
    if (response.status === 401) unauthorized();
    throw error;
  }
  return response;
}

/** The message for anything a request rejected with (v1 errorText). */
export const errorText = (error: unknown) => (error instanceof ApiError ? error.message : serverText("Request failed."));
