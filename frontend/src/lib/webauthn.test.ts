import { afterEach, describe, expect, it, vi } from "vitest";
import { assertionJSON, base64urlDecode, base64urlEncode, createPasskey, getPasskey, PasskeyCancelled, PasskeyFailed, registrationJSON, toCreationOptions, toRequestOptions } from "./webauthn";

const bytes = (...values: number[]) => new Uint8Array(values);

class FakeCredential {
  type = "public-key";
  authenticatorAttachment: string | null = "platform";
  rawId: ArrayBuffer;
  response: object;
  constructor(rawId: ArrayBuffer, response: object) {
    this.rawId = rawId;
    this.response = response;
  }
  getClientExtensionResults() {
    return { credProps: { rk: true } };
  }
}

afterEach(() => vi.unstubAllGlobals());

describe("base64url", () => {
  it("encodes without padding and with the URL-safe alphabet", () => {
    expect(base64urlEncode(bytes(0xfb, 0xff))).toBe("-_8");
    expect(base64urlEncode(bytes(1, 2, 3, 4).buffer)).toBe("AQIDBA");
    expect(base64urlEncode(new Uint8Array(0))).toBe("");
  });

  it("round-trips every byte value", () => {
    const all = new Uint8Array(256).map((_, index) => index);
    expect(Array.from(base64urlDecode(base64urlEncode(all)))).toEqual(Array.from(all));
  });

  it("encodes only the viewed slice of a buffer", () => {
    expect(base64urlEncode(new Uint8Array(bytes(9, 1, 2, 9).buffer, 1, 2))).toBe("AQI");
  });

  it("rejects ordinary base64 and padding", () => {
    expect(() => base64urlDecode("ab+/")).toThrow(TypeError);
    expect(() => base64urlDecode("AQI=")).toThrow(TypeError);
  });
});

describe("options", () => {
  it("decodes creation binaries and passes other fields through", () => {
    const options = toCreationOptions({
      publicKey: {
        rp: { name: "Cove", id: "cove.example.com" },
        user: { id: "dXNlcg", name: "ada@example.com", displayName: "Ada" },
        challenge: "AQID",
        pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        timeout: 300000,
        excludeCredentials: [{ type: "public-key", id: "BAU", transports: ["usb"] }],
        authenticatorSelection: { residentKey: "required", userVerification: "required", requireResidentKey: true },
        attestation: "none",
        extensions: { credProps: true },
      },
    });
    expect(Array.from(options.challenge as Uint8Array)).toEqual([1, 2, 3]);
    expect(new TextDecoder().decode(options.user.id as Uint8Array)).toBe("user");
    expect(Array.from(options.excludeCredentials?.[0]?.id as Uint8Array)).toEqual([4, 5]);
    expect(options.excludeCredentials?.[0]?.transports).toEqual(["usb"]);
    expect(options.authenticatorSelection).toEqual({ residentKey: "required", userVerification: "required", requireResidentKey: true });
    expect(options.extensions).toEqual({ credProps: true });
    expect(options.rp).toEqual({ name: "Cove", id: "cove.example.com" });
  });

  it("decodes request binaries and omits absent allow lists", () => {
    const options = toRequestOptions({ publicKey: { challenge: "AQID", rpId: "cove.example.com", timeout: 300000, userVerification: "required" } });
    expect(Array.from(options.challenge as Uint8Array)).toEqual([1, 2, 3]);
    expect(options).not.toHaveProperty("allowCredentials");
    expect(options.userVerification).toBe("required");
    const scoped = toRequestOptions({ publicKey: { challenge: "AQID", allowCredentials: [{ type: "public-key", id: "BAU" }] } });
    expect(Array.from(scoped.allowCredentials?.[0]?.id as Uint8Array)).toEqual([4, 5]);
  });
});

describe("credential payloads", () => {
  it("builds the registration body the control plane expects", () => {
    const response = { clientDataJSON: bytes(1).buffer, attestationObject: bytes(2).buffer, getTransports: () => ["internal", "hybrid"] };
    const credential = new FakeCredential(bytes(0xfb, 0xff).buffer, response) as unknown as PublicKeyCredential;
    expect(registrationJSON(credential)).toEqual({
      id: "-_8",
      rawId: "-_8",
      type: "public-key",
      response: { clientDataJSON: "AQ", attestationObject: "Ag", transports: ["internal", "hybrid"] },
      clientExtensionResults: { credProps: { rk: true } },
      authenticatorAttachment: "platform",
    });
  });

  it("builds the assertion body and leaves out empty optional fields", () => {
    const response = { clientDataJSON: bytes(1).buffer, authenticatorData: bytes(2).buffer, signature: bytes(3).buffer, userHandle: bytes(4).buffer };
    const fake = new FakeCredential(bytes(5).buffer, response);
    fake.authenticatorAttachment = null;
    expect(assertionJSON(fake as unknown as PublicKeyCredential)).toEqual({
      id: "BQ",
      rawId: "BQ",
      type: "public-key",
      response: { clientDataJSON: "AQ", authenticatorData: "Ag", signature: "Aw", userHandle: "BA" },
      clientExtensionResults: { credProps: { rk: true } },
    });
  });
});

describe("ceremonies", () => {
  const creation = { publicKey: { rp: { name: "Cove" }, user: { id: "AQ", name: "a", displayName: "A" }, challenge: "AQ", pubKeyCredParams: [] } };

  it("treats a closed browser prompt as cancellation rather than failure", async () => {
    vi.stubGlobal("navigator", { credentials: { create: vi.fn().mockRejectedValue(new DOMException("closed", "NotAllowedError")) } });
    await expect(createPasskey(creation)).rejects.toBeInstanceOf(PasskeyCancelled);
  });

  it("explains an authenticator that already holds this account's passkey", async () => {
    vi.stubGlobal("navigator", { credentials: { create: vi.fn().mockRejectedValue(new DOMException("exists", "InvalidStateError")) } });
    await expect(createPasskey(creation)).rejects.toBeInstanceOf(PasskeyFailed);
  });

  it("returns the encoded assertion from the browser", async () => {
    vi.stubGlobal("PublicKeyCredential", FakeCredential);
    const credential = new FakeCredential(bytes(5).buffer, { clientDataJSON: bytes(1).buffer, authenticatorData: bytes(2).buffer, signature: bytes(3).buffer, userHandle: null });
    const get = vi.fn().mockResolvedValue(credential);
    vi.stubGlobal("navigator", { credentials: { get } });
    const result = await getPasskey({ publicKey: { challenge: "AQID" } });
    expect(result.response).not.toHaveProperty("userHandle");
    expect(Array.from((get.mock.calls[0]?.[0] as CredentialRequestOptions).publicKey?.challenge as Uint8Array)).toEqual([1, 2, 3]);
  });
});
