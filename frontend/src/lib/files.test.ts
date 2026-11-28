import { expect, it } from "vitest";
import { decodeBase64, decodeText, emptyRevision, encodeBase64, encodeText, fileNameError, isBlockedPath, joinPath, parentPath, pathSegments, revisionOf, sortEntries, utf8Length } from "./files";

it("round-trips exact bytes through base64, including NUL and high bytes", () => {
  const bytes = Uint8Array.from([0, 1, 127, 128, 200, 255, 10, 13]);
  expect(decodeBase64(encodeBase64(bytes))).toEqual(bytes);
  expect(decodeBase64(null)).toEqual(new Uint8Array());
  expect(decodeBase64("")).toEqual(new Uint8Array());
  const large = Uint8Array.from({ length: 32768 }, (_, index) => index % 256);
  expect(decodeBase64(encodeBase64(large))).toEqual(large);
});

it("encodes text as UTF-8 and decodes only valid UTF-8 text", () => {
  expect(encodeText("café ✓")).toBe(btoa(String.fromCharCode(...new TextEncoder().encode("café ✓"))));
  expect(decodeText(new TextEncoder().encode("line 1\r\nline 2"))).toBe("line 1\r\nline 2");
  expect(decodeText(new Uint8Array())).toBe("");
  expect(decodeText(Uint8Array.from([0xc3, 0x28]))).toBeNull();
  expect(decodeText(Uint8Array.from([0x61, 0x00, 0x62]))).toBeNull();
  // A byte-order mark is kept so saving writes the same bytes back.
  expect(decodeText(Uint8Array.from([0xef, 0xbb, 0xbf, 0x61]))).toBe("﻿a");
});

it("counts the 32 KiB limit in UTF-8 bytes, not characters", () => {
  expect(utf8Length("é")).toBe(2);
  expect(utf8Length("✓".repeat(10923))).toBe(32769);
});

it("matches the server's SHA-256 revisions", async () => {
  expect(await revisionOf(new Uint8Array())).toBe(emptyRevision);
  expect(await revisionOf(new TextEncoder().encode("abc"))).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
});

it("builds clean absolute paths and breadcrumbs", () => {
  expect(joinPath("/", "etc")).toBe("/etc");
  expect(joinPath("/etc", "app.conf")).toBe("/etc/app.conf");
  expect(parentPath("/etc/app")).toBe("/etc");
  expect(parentPath("/etc")).toBe("/");
  expect(parentPath("/")).toBe("/");
  expect(pathSegments("/")).toEqual([]);
  expect(pathSegments("/var/lib/data")).toEqual([
    { name: "var", path: "/var" },
    { name: "lib", path: "/var/lib" },
    { name: "data", path: "/var/lib/data" },
  ]);
});

it("refuses unsafe new names before sending them", () => {
  expect(fileNameError("", "/", "container")).toMatch(/Enter a file name/);
  expect(fileNameError("..", "/", "container")).toMatch(/other than/);
  expect(fileNameError("a/b", "/", "container")).toMatch(/without slashes/);
  expect(fileNameError("a\\b", "/", "container")).toMatch(/backslashes/);
  expect(fileNameError("a\tb", "/", "container")).toMatch(/control characters/);
  expect(fileNameError("a\nb", "/", "volume")).toMatch(/control characters/);
  expect(fileNameError("a\u007fb", "/", "volume")).toMatch(/control characters/);
  expect(fileNameError("a\u0085b", "/", "container")).toMatch(/control characters/);
  expect(fileNameError("café.txt", "/", "container")).toBeUndefined();
  expect(fileNameError("x".repeat(1024), "/", "volume")).toMatch(/1024 bytes/);
  expect(fileNameError("token", "/run/secrets", "container")).toMatch(/secret mounts/);
  expect(fileNameError("token", "/run/secrets", "volume")).toBeUndefined();
  expect(fileNameError(".env", "/app", "container")).toBeUndefined();
});

it("marks special container filesystems as not browsable, but not volume folders", () => {
  expect(isBlockedPath("container", "/proc")).toBe(true);
  expect(isBlockedPath("container", "/dev/shm")).toBe(true);
  expect(isBlockedPath("container", "/process")).toBe(false);
  expect(isBlockedPath("volume", "/proc")).toBe(false);
});

it("lists folders first, then files, links and special files by name", () => {
  const sorted = sortEntries([
    { name: "z.txt", kind: "file", size: 1 },
    { name: "link", kind: "link", size: 0 },
    { name: "b", kind: "directory", size: 0 },
    { name: "a.txt", kind: "file", size: 1 },
    { name: "fifo", kind: "other", size: 0 },
  ]);
  expect(sorted.map((entry) => entry.name)).toEqual(["b", "a.txt", "z.txt", "link", "fifo"]);
});
