import { withBasePath } from "../lib/base-path";
import { selectedTeam } from "../lib/team";

export class ApiError extends Error {
  readonly status: number;
  readonly retryAfter: number | null;
  // A structured error body, for endpoints that explain a refusal with JSON (such as a review whose blockers changed).
  readonly details: unknown;

  constructor(status: number, message: string, retryAfter: number | null = null, details?: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.retryAfter = retryAfter;
    this.details = details;
  }

  get isNetwork() {
    return this.status === 0;
  }
}

const fallbackMessages: Record<number, string> = {
  400: "The request was rejected as invalid.",
  401: "Your session has expired. Sign in again.",
  403: "Access denied. Your account does not have permission for this action.",
  404: "Not found.",
  409: "The request conflicts with the current state.",
  429: "Too many attempts. Try again shortly.",
  500: "The server could not complete the request.",
  502: "The worker could not be reached.",
  503: "The control plane is temporarily unavailable.",
};

function fallbackMessage(status: number) {
  return fallbackMessages[status] ?? `Request failed (HTTP ${status}).`;
}

export const enrolmentRequiredMessage = "Set up two-step verification from your account page to use this team's resources.";

// The control plane reports errors as short plain-text sentences, except the JSON refusal for accounts that
// must enrol a second factor first. Anything else (HTML from a proxy, oversized bodies) gets a generic message.
export async function readErrorMessage(response: Response): Promise<string> {
  const type = response.headers.get("content-type") ?? "";
  if (response.status === 403 && type.startsWith("application/json")) {
    try {
      const body = (await response.json()) as { error?: unknown };
      if (body.error === "second_factor_enrolment_required") return enrolmentRequiredMessage;
    } catch {
      return fallbackMessage(response.status);
    }
    return fallbackMessage(response.status);
  }
  if (!type.startsWith("text/plain")) {
    return fallbackMessage(response.status);
  }
  let text: string;
  try {
    text = (await response.text()).trim();
  } catch {
    return fallbackMessage(response.status);
  }
  if (text === "" || text.length > 300 || text.includes("<")) {
    return fallbackMessage(response.status);
  }
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function parseRetryAfter(response: Response): number | null {
  const value = Number(response.headers.get("retry-after"));
  return Number.isFinite(value) && value > 0 ? value : null;
}

async function readErrorDetails(response: Response): Promise<unknown> {
  if (!(response.headers.get("content-type") ?? "").startsWith("application/json")) return undefined;
  try {
    return await response.clone().json();
  } catch {
    return undefined;
  }
}

interface SendContext {
  teamOverride?: string;
  // Public pages (such as a shared status link) must not carry the visitor's session cookie or selected team.
  anonymous?: boolean;
}

async function send(method: string, path: string, accept: string, body?: unknown, signal?: AbortSignal, context: SendContext = {}): Promise<Response> {
  const headers: Record<string, string> = { Accept: accept };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  return dispatch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal }, context);
}

async function dispatch(path: string, init: { method: string; headers: Record<string, string>; body?: BodyInit; signal?: AbortSignal }, context: SendContext = {}): Promise<Response> {
  const headers = { ...init.headers };
  const team = context.anonymous ? null : (context.teamOverride ?? selectedTeam());
  if (team) {
    headers["X-Cove-Team"] = team;
  }
  let response: Response;
  try {
    response = await fetch(withBasePath(path), {
      method: init.method,
      headers,
      body: init.body,
      credentials: context.anonymous ? "omit" : "same-origin",
      cache: "no-store",
      signal: init.signal,
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") {
      throw error;
    }
    throw new ApiError(0, "Cannot reach the control plane. Check your connection.");
  }
  if (!response.ok) {
    const details = await readErrorDetails(response);
    throw new ApiError(response.status, await readErrorMessage(response), parseRetryAfter(response), details);
  }
  return response;
}

async function readJson<T>(response: Response): Promise<T> {
  try {
    return (await response.json()) as T;
  } catch {
    throw new ApiError(response.status, "The server returned an unreadable response.");
  }
}

export async function request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  return readJson<T>(await send(method, path, "application/json", body, signal));
}

// Reads another team's data, such as destinations for a transfer, without changing the selected team.
export async function requestForTeam<T>(team: string, method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  return readJson<T>(await send(method, path, "application/json", body, signal, { teamOverride: team }));
}

export async function requestPublic<T>(path: string, signal?: AbortSignal): Promise<T> {
  return readJson<T>(await send("GET", path, "application/json", undefined, signal, { anonymous: true }));
}

// Sends a file as the raw request body. The browser streams a File from disk, so it is never read into memory here.
export async function uploadFile<T>(path: string, file: Blob, signal?: AbortSignal, extraHeaders: Record<string, string> = {}): Promise<T> {
  const headers = { ...extraHeaders, Accept: "application/json", "Content-Type": "application/octet-stream", "X-Cove-Size": String(file.size) };
  return readJson<T>(await dispatch(path, { method: "POST", headers, body: file, signal }));
}

// Plain-text GET for exports, sent with the same session and team headers as JSON requests.
export async function requestText(path: string, signal?: AbortSignal): Promise<string> {
  const response = await send("GET", path, "text/plain", undefined, signal);
  try {
    return await response.text();
  } catch {
    throw new ApiError(response.status, "The server returned an unreadable response.");
  }
}

// For attachments such as archives. The caller saves the blob and drops it; nothing is cached.
export async function requestFile(method: string, path: string, body?: unknown): Promise<{ blob: Blob; filename: string | null }> {
  const response = await send(method, path, "*/*", body);
  const disposition = response.headers.get("content-disposition") ?? "";
  const filename = /filename="?([^";]+)"?/i.exec(disposition)?.[1]?.trim() ?? null;
  try {
    return { blob: await response.blob(), filename };
  } catch {
    throw new ApiError(response.status, "The server returned an unreadable response.");
  }
}

// Binary GET for audited downloads, sent with the same session and team headers as JSON requests.
export async function requestBlob(path: string, signal?: AbortSignal): Promise<Blob> {
  const response = await send("GET", path, "application/octet-stream", undefined, signal);
  try {
    return await response.blob();
  } catch {
    throw new ApiError(response.status, "The server returned an unreadable response.");
  }
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    return error.message;
  }
  return "Something went wrong.";
}

export function isStatus(error: unknown, status: number) {
  return error instanceof ApiError && error.status === status;
}
