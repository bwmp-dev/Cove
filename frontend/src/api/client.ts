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

async function send(method: string, path: string, accept: string, body?: unknown, signal?: AbortSignal, teamOverride?: string, anonymous = false): Promise<Response> {
  const headers: Record<string, string> = { Accept: accept };
  const team = anonymous ? null : (teamOverride ?? selectedTeam());
  if (team) {
    headers["X-Cove-Team"] = team;
  }
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  let response: Response;
  try {
    response = await fetch(withBasePath(path), {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: anonymous ? "omit" : "same-origin",
      cache: "no-store",
      signal,
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

export function request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  return requestJSON<T>(method, path, body, signal);
}

// Reads another team's data, such as destinations for a transfer, without changing the selected team.
export function requestForTeam<T>(team: string, method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  return requestJSON<T>(method, path, body, signal, team);
}

async function requestJSON<T>(method: string, path: string, body: unknown, signal: AbortSignal | undefined, team?: string): Promise<T> {
  const response = await send(method, path, "application/json", body, signal, team);
  try {
    return (await response.json()) as T;
  } catch {
    throw new ApiError(response.status, "The server returned an unreadable response.");
  }
}

// Public pages (such as a shared status link) must not carry the visitor's session cookie or selected team.
export async function requestPublic<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await send("GET", path, "application/json", undefined, signal, undefined, true);
  try {
    return (await response.json()) as T;
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

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    return error.message;
  }
  return "Something went wrong.";
}

export function isStatus(error: unknown, status: number) {
  return error instanceof ApiError && error.status === status;
}
