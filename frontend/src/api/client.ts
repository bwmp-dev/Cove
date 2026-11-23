import { selectedTeam } from "../lib/team";

export class ApiError extends Error {
  readonly status: number;
  readonly retryAfter: number | null;

  constructor(status: number, message: string, retryAfter: number | null = null) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.retryAfter = retryAfter;
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

// The control plane reports errors as short plain-text sentences. Anything else
// (HTML from a proxy, oversized bodies) is replaced by a generic message.
export async function readErrorMessage(response: Response): Promise<string> {
  const type = response.headers.get("content-type") ?? "";
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

async function send(method: string, path: string, accept: string, body?: unknown, signal?: AbortSignal): Promise<Response> {
  const headers: Record<string, string> = { Accept: accept };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  return dispatch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal });
}

async function dispatch(path: string, init: { method: string; headers: Record<string, string>; body?: BodyInit; signal?: AbortSignal }): Promise<Response> {
  const headers = { ...init.headers };
  const team = selectedTeam();
  if (team) {
    headers["X-Cove-Team"] = team;
  }
  let response: Response;
  try {
    response = await fetch(path, {
      method: init.method,
      headers,
      body: init.body,
      credentials: "same-origin",
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
    throw new ApiError(response.status, await readErrorMessage(response), parseRetryAfter(response));
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
