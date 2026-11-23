import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import { reauthErrorMessage, reauthMethods, reauthProof } from "./reauth";

describe("re-authentication", () => {
  it("offers the proofs this account can give, always including a recent sign-in", () => {
    expect(reauthMethods(true, { totp_enrolled: true, recovery_codes_remaining: 10 })).toEqual(["password", "code", "recent"]);
    expect(reauthMethods(false, { totp_enrolled: false, recovery_codes_remaining: 3 })).toEqual(["code", "recent"]);
    expect(reauthMethods(false, { totp_enrolled: false, recovery_codes_remaining: 0 })).toEqual(["recent"]);
  });

  it("builds the request body for each proof", () => {
    expect(reauthProof("password", "secret")).toEqual({ proof: { current_password: "secret" } });
    expect(reauthProof("code", " 123 456 ")).toEqual({ proof: { code: "123456" } });
    expect(reauthProof("code", "abcd-efgh")).toEqual({ proof: { code: "abcd-efgh" } });
    expect(reauthProof("recent", "")).toEqual({ proof: {} });
    expect(reauthProof("password", "")).toHaveProperty("error");
    expect(reauthProof("code", "  ")).toHaveProperty("error");
  });

  it("explains a rejected proof in terms of the method used", () => {
    const rejected = new ApiError(403, "Reauthentication required");
    expect(reauthErrorMessage(rejected, "password")).toBe("The password is incorrect.");
    expect(reauthErrorMessage(rejected, "code")).toMatch(/already used/);
    expect(reauthErrorMessage(rejected, "recent")).toMatch(/ten minutes/);
    expect(reauthErrorMessage(new ApiError(429, "Too many attempts", 60), "password")).toBe("Too many attempts. Try again in 60 seconds.");
    expect(reauthErrorMessage(new ApiError(409, "Second factor not enrolled"), "password")).toBe("Second factor not enrolled");
  });
});
