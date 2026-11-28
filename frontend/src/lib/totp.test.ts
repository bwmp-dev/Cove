import { describe, expect, it } from "vitest";
import { groupSecret, parseProvisioningURI } from "./totp";

describe("parseProvisioningURI", () => {
  it("reads the key, issuer and account from the server URI", () => {
    const uri = "otpauth://totp/Cove%3Aada%40example.com?algorithm=SHA1&digits=6&issuer=Cove&period=30&secret=JBSWY3DPEHPK3PXP";
    expect(parseProvisioningURI(uri)).toEqual({ secret: "JBSWY3DPEHPK3PXP", issuer: "Cove", account: "ada@example.com" });
  });

  it("rejects other schemes and malformed secrets", () => {
    expect(parseProvisioningURI("https://example.com/?secret=JBSWY3DP")).toBeNull();
    expect(parseProvisioningURI("otpauth://totp/Cove?secret=not-base32!")).toBeNull();
    expect(parseProvisioningURI("not a uri")).toBeNull();
  });

  it("groups the manual key for reading", () => {
    expect(groupSecret("JBSWY3DPEHPK3PXP")).toBe("JBSW Y3DP EHPK 3PXP");
  });
});
