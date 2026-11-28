import { expect, it } from "vitest";
import { b2PathError, storageFromForm, storageSummary } from "./storage";

const form = (values: Record<string, string>) => (key: string) => values[key] ?? "";

it("serialises Backblaze B2 without an endpoint, CA or fields from other protocols", () => {
  const storage = storageFromForm("b2", form({ path: " backups/prod ", access_key: " 0012ab ", secret_key: "K001secret", endpoint: "https://s3.example.com", ca_certificate: "PEM", username: "old", password: "old", region: "us", host_key: "ssh-ed25519 A", tls_mode: "explicit" }));
  expect(storage).toEqual({ protocol: "b2", endpoint: "", path: "backups/prod", access_key: "0012ab", secret_key: "K001secret" });
});

it("keeps a blank application key on edit so the API can preserve the sealed key", () => {
  expect(storageFromForm("b2", form({ path: "backups", access_key: "0012ab" })).secret_key).toBe("");
});

it("serialises existing protocols with their previous fields", () => {
  expect(storageFromForm("sftp", form({ endpoint: " sftp://host:22 ", path: "dir", username: "u", password: "", host_key: " ssh-ed25519 A " }))).toEqual({ ca_certificate: "", protocol: "sftp", endpoint: "sftp://host:22", path: "dir", username: "u", password: "", access_key: "", secret_key: "", region: "", host_key: "ssh-ed25519 A", tls_mode: "" });
});

it("rejects B2 paths the worker refuses", () => {
  expect(b2PathError("bucket/path")).toBeUndefined();
  expect(b2PathError("")).toBeDefined();
  expect(b2PathError("/bucket")).toMatch(/leading slash/);
  expect(b2PathError("bucket/../other")).toBeDefined();
  expect(b2PathError("bucket\\path")).toBeDefined();
  expect(b2PathError("bucket:path")).toBeDefined();
  expect(b2PathError("bucket\npath")).toBeDefined();
});

it("summarises locations without empty endpoints", () => {
  expect(storageSummary({ protocol: "b2", endpoint: "", path: "bucket/path" })).toBe("Backblaze B2 · bucket/path");
  expect(storageSummary({ protocol: "webdav", endpoint: "https://dav", path: "dir" })).toBe("WebDAV · https://dav · dir");
});
