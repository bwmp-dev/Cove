import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import { definiteRefusal, removedPorts, selectablePorts, validPrefix, validateDrafts } from "./firewall";

const ports = [
  { protocol: "tcp" as const, port: 8080 },
  { protocol: "udp" as const, port: 8080 },
  { protocol: "tcp" as const, port: 2222 },
];

describe("firewall helpers", () => {
  it("excludes protected management ports for every protocol", () => {
    expect(selectablePorts(ports, [22, 2222])).toEqual(ports.slice(0, 2));
    expect(selectablePorts(ports, [8080])).toEqual([{ protocol: "tcp", port: 2222 }]);
  });

  it("accepts IPv4 and IPv6 prefixes only", () => {
    for (const value of ["192.0.2.0/24", "0.0.0.0/0", "2001:db8::/32", "::/0"]) expect(validPrefix(value)).toBe(true);
    for (const value of ["192.0.2.1", "192.0.2.0/33", "256.0.0.0/8", "example.com/24", "fe80::1%eth0/64", "2001:db8::/129"]) expect(validPrefix(value)).toBe(false);
  });

  it("rejects duplicate, unpublished and oversized rules", () => {
    const duplicate = validateDrafts(
      [
        { id: 1, port: "tcp/8080", sources: "" },
        { id: 2, port: "tcp/8080", sources: "" },
      ],
      ports,
    );
    expect(duplicate.rules).toBeUndefined();
    expect(duplicate.errors.get(2)?.port).toMatch(/only one rule/);
    expect(validateDrafts([{ id: 1, port: "tcp/9000", sources: "" }], ports).errors.get(1)?.port).toMatch(/no longer a published/);
    const many = Array.from({ length: 33 }, (_, index) => `10.0.${index}.0/24`).join("\n");
    expect(validateDrafts([{ id: 1, port: "udp/8080", sources: many }], ports).errors.get(1)?.sources).toMatch(/At most 32/);
    expect(validateDrafts([{ id: 1, port: "udp/8080", sources: "10.0.0.0/8, 192.0.2.0/24" }], ports).rules).toEqual([{ protocol: "udp", port: 8080, sources: ["10.0.0.0/8", "192.0.2.0/24"] }]);
  });

  it("names ports whose Cove rule is removed", () => {
    const current = [
      { protocol: "tcp" as const, port: 8080, sources: [] },
      { protocol: "udp" as const, port: 8080, sources: [] },
    ];
    expect(removedPorts(current, [{ protocol: "tcp", port: 8080, sources: ["10.0.0.0/8"] }])).toEqual([current[1]]);
  });

  it("treats only pre-worker rejections as definite", () => {
    expect(definiteRefusal(new ApiError(400, "x"))).toBe(true);
    expect(definiteRefusal(new ApiError(409, "x"))).toBe(false);
    expect(definiteRefusal(new ApiError(502, "x"))).toBe(false);
    expect(definiteRefusal(new Error("network"))).toBe(false);
  });
});
