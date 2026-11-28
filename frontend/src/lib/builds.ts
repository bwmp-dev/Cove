import type { Operation, Workload } from "../api/types";
import { workerResult } from "./operations";
import { isManagedPreview } from "./previews";

const repositoryPattern = /^[a-z0-9]+([._/-][a-z0-9]+)*$/;
export const repositoryMaxLength = 128;

export function repositoryError(value: string) {
  if (value === "") return "Enter a repository path.";
  if (value.length > repositoryMaxLength) return `Use at most ${repositoryMaxLength} characters.`;
  if (value.includes(":") || value.includes("@")) return "Enter the repository path only, without a registry host, tag or digest.";
  if (!repositoryPattern.test(value)) return "Use lowercase letters and digits, separated by single dots, dashes, underscores or slashes.";
  return null;
}

// The API rejects builds for managed previews, so only regular services and jobs build.
export function supportsBuildPublish(workload: Workload) {
  return !isManagedPreview(workload);
}

// The list endpoint returns operations newest first. The latest success is kept separately so its
// published references stay visible while a newer build runs or after it fails.
export function buildHistory(operations: Operation[], workloadId: string) {
  const builds = operations.filter((operation) => operation.workload_id === workloadId && operation.action === "build");
  const success = builds.find((operation) => operation.state === "succeeded" && workerResult(operation)?.build);
  return { attempt: builds[0], success };
}
