import { describe, expect, it } from "vitest";
import { authorizedKeyLine, bastionEndpoint, hostKeyProblem, identityActions, sshdMatchBlock, validateBastion } from "./bastions";

const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl root@jump";

describe("validateBastion", () => {
  it("accepts a pinned host public key and trims values", () => {
    expect(validateBastion({ name: " Jump ", address: "10.0.0.5", port: "2222", user: "cove-jump", host_key: ` ${hostKey}\n` })).toEqual({
      input: { name: "Jump", address: "10.0.0.5", port: 2222, user: "cove-jump", host_key: hostKey },
      errors: {},
    });
  });

  it("rejects invalid endpoints and forwarding users", () => {
    const { input, errors } = validateBastion({ name: "", address: "a b", port: "70000", user: "Root", host_key: hostKey });
    expect(input).toBeUndefined();
    expect(Object.keys(errors).sort()).toEqual(["address", "name", "port", "user"]);
  });
});

describe("hostKeyProblem", () => {
  it("refuses private keys, known_hosts lines and several keys", () => {
    expect(hostKeyProblem("-----BEGIN OPENSSH PRIVATE KEY-----\nabc")).toMatch(/private key/);
    expect(hostKeyProblem(`jump.example ${hostKey}`)).toMatch(/known_hosts/);
    expect(hostKeyProblem(`${hostKey}\n${hostKey}`)).toMatch(/exactly one/);
    expect(hostKeyProblem("not a key")).toBeDefined();
    expect(hostKeyProblem(hostKey)).toBeUndefined();
  });
});

describe("bastion configuration", () => {
  const bastion = { user: "cove-jump", public_key: "ssh-ed25519 AAAAgenerated cove-bastion", targets: ["10.0.0.12:22", "[fd00::5]:2222"] };

  it("permits only the assigned destinations", () => {
    expect(sshdMatchBlock(bastion)).toContain("    PermitOpen 10.0.0.12:22 [fd00::5]:2222");
    expect(sshdMatchBlock(bastion)).toContain("AllowTcpForwarding local");
    expect(sshdMatchBlock(bastion)).toContain("ForceCommand /bin/false");
    expect(authorizedKeyLine(bastion)).toBe('restrict,port-forwarding,permitopen="10.0.0.12:22",permitopen="[fd00::5]:2222",command="/bin/false" ssh-ed25519 AAAAgenerated cove-bastion');
  });

  it("permits nothing when no node uses the bastion", () => {
    const empty = { ...bastion, targets: [] };
    expect(sshdMatchBlock(empty)).toContain("PermitOpen none");
    expect(authorizedKeyLine(empty)).toBe('restrict,command="/bin/false" ssh-ed25519 AAAAgenerated cove-bastion');
    expect(authorizedKeyLine(empty)).not.toContain("port-forwarding");
  });

  it("brackets IPv6 endpoints", () => {
    expect(bastionEndpoint({ user: "jump", address: "fd00::1", port: 22 })).toBe("jump@[fd00::1]:22");
  });
});

it("offers cancel only before the new key is saved", () => {
  expect(identityActions("idle")).toEqual(["rotate"]);
  expect(identityActions("succeeded")).toEqual(["rotate"]);
  expect(identityActions("cancelled")).toEqual(["rotate"]);
  expect(identityActions("queued")).toEqual(["retry", "cancel"]);
  expect(identityActions("staged")).toEqual(["retry", "cancel"]);
  expect(identityActions("switched")).toEqual(["retry"]);
  expect(identityActions("aborting")).toEqual(["retry"]);
});
