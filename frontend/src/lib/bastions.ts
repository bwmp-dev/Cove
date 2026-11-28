import type { Bastion, BastionInput, IdentityAction, IdentityState } from "../api/bastions";
import type { Tone } from "./nodes";

export type BastionErrors = Partial<Record<keyof BastionInput, string>>;

const loginPattern = /^[a-z_][a-z0-9_-]{0,31}$/;
const hostKeyTypes = ["ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com"];
const publicKeyLine = /^(\S+) [A-Za-z0-9+/]+={0,3}(?: [^\r\n]*)?$/;

// The API parses the key authoritatively; this catches the common wrong pastes before sending.
export function hostKeyProblem(value: string): string | undefined {
  const key = value.trim();
  if (key === "") return "Paste the bastion's OpenSSH host public key.";
  if (/PRIVATE KEY/.test(key)) return "This is a private key. Paste only the host public key, such as the contents of /etc/ssh/ssh_host_ed25519_key.pub.";
  if (/[\r\n]/.test(key)) return "Paste exactly one host public key on a single line.";
  const parts = key.split(" ");
  if (!hostKeyTypes.includes(parts[0] ?? "") && hostKeyTypes.includes(parts[1] ?? "")) return "This looks like a known_hosts line. Remove the leading host name so the line starts with the key type.";
  const match = publicKeyLine.exec(key);
  if (!match || !hostKeyTypes.includes(match[1] ?? "")) return "Enter one OpenSSH public key line: the key type, the base64 key and an optional comment.";
  return undefined;
}

export function validateBastion(values: { name: string; address: string; port: string; user: string; host_key: string }): { input?: BastionInput; errors: BastionErrors } {
  const errors: BastionErrors = {};
  const name = values.name.trim();
  const address = values.address.trim();
  const user = values.user.trim();
  const portText = values.port.trim();
  const port = Number(portText);
  if (name === "") errors.name = "Enter a name.";
  else if (name.length > 120) errors.name = "Use at most 120 characters.";
  if (address === "") errors.address = "Enter the bastion's hostname or IP address.";
  else if (address.length > 253 || /[\s/\\"']/.test(address)) errors.address = "Enter a hostname or IP address without spaces, slashes, or quotes.";
  if (!/^\d+$/.test(portText) || port < 1 || port > 65535) errors.port = "Enter a port from 1 to 65535.";
  if (!loginPattern.test(user)) errors.user = "Enter a Linux user name: lowercase letters, digits, _ or -, starting with a letter or _, at most 32 characters.";
  const keyProblem = hostKeyProblem(values.host_key);
  if (keyProblem) errors.host_key = keyProblem;
  if (Object.keys(errors).length > 0) return { errors };
  return { input: { name, address, port, user, host_key: values.host_key.trim() }, errors };
}

export function bastionEndpoint(bastion: Pick<Bastion, "user" | "address" | "port">) {
  const host = bastion.address.includes(":") ? `[${bastion.address}]` : bastion.address;
  return `${bastion.user}@${host}:${bastion.port}`;
}

// sshd settings for the dedicated forwarding account. A bastion with no assigned nodes permits no
// destinations rather than every destination.
export function sshdMatchBlock(bastion: Pick<Bastion, "user" | "targets">) {
  return [
    `Match User ${bastion.user}`,
    "    AllowTcpForwarding local",
    "    AllowStreamLocalForwarding no",
    "    AllowAgentForwarding no",
    "    X11Forwarding no",
    "    PermitTTY no",
    "    PermitTunnel no",
    "    GatewayPorts no",
    "    PermitListen none",
    "    ForceCommand /bin/false",
    `    PermitOpen ${bastion.targets.length > 0 ? bastion.targets.join(" ") : "none"}`,
  ].join("\n");
}

export function authorizedKeyLine(bastion: Pick<Bastion, "public_key" | "targets">) {
  const options = bastion.targets.length > 0 ? ["restrict", "port-forwarding", ...bastion.targets.map((target) => `permitopen="${target}"`), 'command="/bin/false"'] : ["restrict", 'command="/bin/false"'];
  return `${options.join(",")} ${bastion.public_key}`;
}

export const identityStates: Record<IdentityState, { label: string; tone: Tone; description: string }> = {
  idle: { label: "Not rotated", tone: "neutral", description: "The node uses the SSH key created when it was added. Rotating replaces it with a new restricted key." },
  queued: { label: "Waiting to start", tone: "info", description: "Cove will add a new restricted key to the worker while the current key keeps working." },
  staged: { label: "Proving the new key", tone: "info", description: "The new key is installed beside the current one. Cove saves it only after a fresh SSH and worker connection succeeds with it." },
  switched: { label: "Removing the old key", tone: "info", description: "The new key is saved and in use. Cove is removing the old key from the worker and waiting for confirmation. This step cannot be cancelled." },
  aborting: { label: "Cancelling", tone: "warn", description: "Cove is removing the new key from the worker. The current key stays in use." },
  succeeded: { label: "Rotated", tone: "ok", description: "The new key is saved and the old key was removed from the worker." },
  cancelled: { label: "Cancelled", tone: "neutral", description: "The new key was removed and the node kept its previous key." },
};

export function identityActions(state: IdentityState): IdentityAction[] {
  if (state === "queued" || state === "staged") return ["retry", "cancel"];
  if (state === "switched" || state === "aborting") return ["retry"];
  return ["rotate"];
}
