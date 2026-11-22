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

export async function request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  const team = selectedTeam();
  if (team) {
    headers["X-Cove-Team"] = team;
  }
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  let response: Response;
  try {
    response = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: "same-origin",
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
    throw new ApiError(response.status, await readErrorMessage(response), parseRetryAfter(response));
  }
  try {
    return (await response.json()) as T;
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
