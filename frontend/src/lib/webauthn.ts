// The control plane speaks the WebAuthn JSON forms with unpadded base64url for every binary field.
type Bytes = Uint8Array<ArrayBuffer>;

export interface CredentialDescriptorJSON {
  type: "public-key";
  id: string;
  transports?: AuthenticatorTransport[];
}

export interface CreationOptionsJSON {
  publicKey: Omit<PublicKeyCredentialCreationOptions, "challenge" | "user" | "excludeCredentials"> & {
    challenge: string;
    user: { id: string; name: string; displayName: string };
    excludeCredentials?: CredentialDescriptorJSON[];
  };
}

export interface RequestOptionsJSON {
  publicKey: Omit<PublicKeyCredentialRequestOptions, "challenge" | "allowCredentials"> & {
    challenge: string;
    allowCredentials?: CredentialDescriptorJSON[];
  };
}

export interface RegistrationJSON {
  id: string;
  rawId: string;
  type: "public-key";
  response: { clientDataJSON: string; attestationObject: string; transports?: string[] };
  clientExtensionResults: AuthenticationExtensionsClientOutputs;
  authenticatorAttachment?: string;
}

export interface AssertionJSON {
  id: string;
  rawId: string;
  type: "public-key";
  response: { clientDataJSON: string; authenticatorData: string; signature: string; userHandle?: string };
  clientExtensionResults: AuthenticationExtensionsClientOutputs;
  authenticatorAttachment?: string;
}

export function base64urlEncode(value: ArrayBuffer | ArrayBufferView): string {
  const bytes = value instanceof ArrayBuffer ? new Uint8Array(value) : new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function base64urlDecode(value: string): Bytes {
  if (!/^[A-Za-z0-9_-]*$/.test(value)) throw new TypeError("Invalid base64url value");
  const base64 = value.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(value.length / 4) * 4, "=");
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

function descriptor(item: CredentialDescriptorJSON): PublicKeyCredentialDescriptor {
  return { ...item, id: base64urlDecode(item.id) };
}

// Unknown option fields from the server library are passed through untouched.
export function toCreationOptions({ publicKey }: CreationOptionsJSON): PublicKeyCredentialCreationOptions {
  const { excludeCredentials, ...rest } = publicKey;
  return {
    ...rest,
    challenge: base64urlDecode(publicKey.challenge),
    user: { ...publicKey.user, id: base64urlDecode(publicKey.user.id) },
    ...(excludeCredentials ? { excludeCredentials: excludeCredentials.map(descriptor) } : {}),
  };
}

export function toRequestOptions({ publicKey }: RequestOptionsJSON): PublicKeyCredentialRequestOptions {
  const { allowCredentials, ...rest } = publicKey;
  return {
    ...rest,
    challenge: base64urlDecode(publicKey.challenge),
    ...(allowCredentials ? { allowCredentials: allowCredentials.map(descriptor) } : {}),
  };
}

function attachment(credential: PublicKeyCredential) {
  return credential.authenticatorAttachment ? { authenticatorAttachment: credential.authenticatorAttachment } : {};
}

export function registrationJSON(credential: PublicKeyCredential): RegistrationJSON {
  const response = credential.response as AuthenticatorAttestationResponse;
  const transports = typeof response.getTransports === "function" ? response.getTransports() : [];
  const rawId = base64urlEncode(credential.rawId);
  return {
    id: rawId,
    rawId,
    type: "public-key",
    response: {
      clientDataJSON: base64urlEncode(response.clientDataJSON),
      attestationObject: base64urlEncode(response.attestationObject),
      ...(transports.length > 0 ? { transports } : {}),
    },
    clientExtensionResults: credential.getClientExtensionResults(),
    ...attachment(credential),
  };
}

export function assertionJSON(credential: PublicKeyCredential): AssertionJSON {
  const response = credential.response as AuthenticatorAssertionResponse;
  const rawId = base64urlEncode(credential.rawId);
  return {
    id: rawId,
    rawId,
    type: "public-key",
    response: {
      clientDataJSON: base64urlEncode(response.clientDataJSON),
      authenticatorData: base64urlEncode(response.authenticatorData),
      signature: base64urlEncode(response.signature),
      ...(response.userHandle ? { userHandle: base64urlEncode(response.userHandle) } : {}),
    },
    clientExtensionResults: credential.getClientExtensionResults(),
    ...attachment(credential),
  };
}

export function passkeysSupported() {
  return typeof window !== "undefined" && typeof window.PublicKeyCredential === "function" && Boolean(navigator.credentials);
}

// Closing the browser prompt is the person's choice, not a failure to report as an error.
export class PasskeyCancelled extends Error {
  constructor() {
    super("The passkey prompt was closed.");
    this.name = "PasskeyCancelled";
  }
}

export class PasskeyFailed extends Error {
  constructor(message: string) {
    super(message);
    this.name = "PasskeyFailed";
  }
}

function browserFailure(error: unknown, registering: boolean): Error {
  if (error instanceof DOMException) {
    if (error.name === "NotAllowedError" || error.name === "AbortError") return new PasskeyCancelled();
    if (error.name === "InvalidStateError" && registering) return new PasskeyFailed("This authenticator already holds a passkey for your account.");
    if (error.name === "SecurityError") return new PasskeyFailed("The browser refused the request because this page is not served from the configured public URL (COVE_PUBLIC_URL).");
    if (error.name === "NotSupportedError") return new PasskeyFailed("This browser or authenticator does not support the passkey options Cove requires (a discoverable credential with user verification).");
  }
  return new PasskeyFailed("The browser could not complete the passkey request.");
}

export async function createPasskey(options: CreationOptionsJSON): Promise<RegistrationJSON> {
  let credential: Credential | null;
  try {
    credential = await navigator.credentials.create({ publicKey: toCreationOptions(options) });
  } catch (error) {
    throw browserFailure(error, true);
  }
  if (!(credential instanceof PublicKeyCredential)) throw new PasskeyCancelled();
  return registrationJSON(credential);
}

export async function getPasskey(options: RequestOptionsJSON): Promise<AssertionJSON> {
  let credential: Credential | null;
  try {
    credential = await navigator.credentials.get({ publicKey: toRequestOptions(options) });
  } catch (error) {
    throw browserFailure(error, false);
  }
  if (!(credential instanceof PublicKeyCredential)) throw new PasskeyCancelled();
  return assertionJSON(credential);
}
