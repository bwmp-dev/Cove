import { expect, it } from "vitest";
import { ApiError } from "../api/client";
import { remediationFor } from "./error-remediation";

it("suggests upgrading the worker for capability refusals", () => {
  expect(remediationFor(new ApiError(409, "Upgrade and verify the worker before deploying previews"))).toEqual({
    fix: "Upgrade the worker from its node page, then verify the node.",
    link: { label: "Open nodes", href: "/nodes" },
  });
});

it("links busy and unreachable workers to the current resource when one is open", () => {
  expect(remediationFor(new ApiError(409, "Wait for the active operation before editing"), "/resources/abc123/domains")?.link).toEqual({ label: "Open resource operations", href: "/resources/abc123/operations" });
  expect(remediationFor(new ApiError(409, "Workload already has an active operation"))?.link).toEqual({ label: "Open operations", href: "/operations" });
  expect(remediationFor(new ApiError(502, "The worker could not be reached."), "/resources/abc123")?.link).toEqual({ label: "Open troubleshooting", href: "/resources/abc123/troubleshooting" });
  expect(remediationFor(new ApiError(503, "Worker unavailable; verify the node and upgrade its worker if this persists"))?.link).toEqual({ label: "Open nodes", href: "/nodes" });
});

it("does not build links from unexpected path segments", () => {
  expect(remediationFor(new ApiError(502, "Worker rejected request"), "/resources/..%2Fevil/logs")?.link).toEqual({ label: "Open nodes", href: "/nodes" });
});

it("maps session, permission, rate-limit, quota and integration errors", () => {
  expect(remediationFor(new ApiError(401, "Your session has expired. Sign in again."))?.link?.href).toBe("/login");
  expect(remediationFor(new ApiError(403, "Permission denied"))?.fix).toBe("Ask a team administrator for access to this action.");
  expect(remediationFor(new ApiError(429, "Too many attempts. Try again shortly.", 1))?.fix).toBe("Try again in 1 second.");
  expect(remediationFor(new ApiError(429, "Too many attempts. Try again shortly.", 30))?.fix).toBe("Try again in 30 seconds.");
  expect(remediationFor(new ApiError(429, "Too many attempts. Try again shortly."))).toBeNull();
  expect(remediationFor(new ApiError(409, "Quota exceeded; ask an instance administrator to adjust resource or configured-limit budgets"))?.link?.href).toBe("/team");
  expect(remediationFor(new ApiError(502, "Cloudflare could not be reached"))?.link?.href).toBe("/integrations");
  expect(remediationFor(new ApiError(503, "GitHub reporting unavailable"))?.link?.href).toBe("/integrations");
  expect(remediationFor(new ApiError(403, "Origin rejected"))?.fix).toBe("Open Cove at its configured public address and try again.");
  expect(remediationFor(new ApiError(503, "Database unavailable"))?.link).toBeUndefined();
});

it("leaves unknown, network and non-API errors without a suggestion", () => {
  expect(remediationFor(new ApiError(400, "Name must contain 1-120 characters"))).toBeNull();
  expect(remediationFor(new ApiError(0, "Cannot reach the control plane. Check your connection."))).toBeNull();
  expect(remediationFor(new Error("upgrade and verify the worker"))).toBeNull();
  expect(remediationFor("permission denied")).toBeNull();
});
