export interface Provisioning {
  secret: string;
  issuer: string;
  account: string;
}

// Reads the manual-entry key from the otpauth:// URI returned once at enrolment. Never persist the result.
export function parseProvisioningURI(uri: string): Provisioning | null {
  let url: URL;
  try {
    url = new URL(uri);
  } catch {
    return null;
  }
  if (url.protocol !== "otpauth:") return null;
  const secret = url.searchParams.get("secret") ?? "";
  if (!/^[A-Z2-7]+$/i.test(secret)) return null;
  const label = decodeURIComponent(url.pathname.replace(/^\/\/totp\//, "").replace(/^totp\//, "").replace(/^\//, ""));
  const separator = label.indexOf(":");
  return {
    secret: secret.toUpperCase(),
    issuer: url.searchParams.get("issuer") ?? (separator >= 0 ? label.slice(0, separator) : ""),
    account: separator >= 0 ? label.slice(separator + 1) : label,
  };
}

export function groupSecret(secret: string) {
  return secret.match(/.{1,4}/g)?.join(" ") ?? "";
}
