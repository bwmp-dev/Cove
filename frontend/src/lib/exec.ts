import { parse } from "yaml";
import { byteLength } from "./workload-form";

// Limits enforced by the control plane and worker (internal/worker/exec.go).
export const execLimits = { maxArguments: 64, maxArgumentBytes: 4096, maxTotalBytes: 16384, maxTimeoutSeconds: 30 } as const;

export function commandProblem(command: readonly string[]): string | null {
  if (command.length === 0 || command[0] === "") return "Enter the program to run as the first argument.";
  if (command.length > execLimits.maxArguments) return `Use at most ${execLimits.maxArguments} arguments.`;
  let total = 0;
  for (const [index, argument] of command.entries()) {
    // Matches the form labels: the program, then Argument 1, 2, ...
    const name = index === 0 ? "The program" : `Argument ${index}`;
    const size = byteLength(argument);
    if (argument.includes("\0")) return `${name} contains a NUL character.`;
    if (size > execLimits.maxArgumentBytes) return `${name} is longer than ${execLimits.maxArgumentBytes} bytes.`;
    total += size;
  }
  if (total > execLimits.maxTotalBytes) return `The command is longer than ${execLimits.maxTotalBytes / 1024} KiB in total.`;
  return null;
}

export function timeoutProblem(seconds: number): string | null {
  if (!Number.isInteger(seconds) || seconds < 1 || seconds > execLimits.maxTimeoutSeconds) return `Enter a whole number of seconds from 1 to ${execLimits.maxTimeoutSeconds}.`;
  return null;
}

// An unreadable or empty Compose file simply offers no choices.
export function composeServices(compose: string): string[] {
  try {
    const document: unknown = parse(compose);
    if (!document || typeof document !== "object" || !("services" in document)) return [];
    const services = document.services;
    if (!services || typeof services !== "object" || Array.isArray(services)) return [];
    return Object.keys(services);
  } catch {
    return [];
  }
}

export const containerIdPattern = /^[a-f0-9]{12,64}$/;
