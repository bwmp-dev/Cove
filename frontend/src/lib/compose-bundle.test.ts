import { describe, expect, it } from "vitest";
import { draftFromBundle, fileRow, move, sameDraft, validateBundle, validateRelativePath, type BundleDraft } from "./compose-bundle";

const empty: BundleDraft = { files: [], assets: [], paths: [], profiles: "" };

describe("validateRelativePath", () => {
  it.each(["/etc/compose.yaml", "a\\b.yaml", "c:/x.yaml", "a//b.yaml", "a/", "./a.yaml", "../a.yaml", ".git/config", "x/.cove-secret/a", ""])("rejects %s", (value) => {
    expect(validateRelativePath(value)).not.toBeNull();
  });
  it.each(["production.yaml", "deploy/env/base.env", ".env", "a.b/c-d_e.yml"])("accepts %s", (value) => {
    expect(validateRelativePath(value)).toBeNull();
  });
});

describe("validateBundle", () => {
  it("keeps override order and maps ancillary files", () => {
    const draft = { ...empty, files: [fileRow("b.yaml", "services: {}\n"), fileRow("a.yaml", "x: 1\n")], assets: [fileRow("base.env", "KEY=1\n")], profiles: "worker, metrics" };
    expect(validateBundle(draft, "inline", "compose.yaml").bundle).toEqual({
      files: [{ path: "b.yaml", content: "services: {}\n" }, { path: "a.yaml", content: "x: 1\n" }],
      assets: { "base.env": "KEY=1\n" },
      paths: [],
      profiles: ["worker", "metrics"],
    });
  });

  it("rejects the primary name and case-insensitive duplicates", () => {
    const primary = fileRow("Compose.YAML", "x");
    const first = fileRow("prod.yaml", "x");
    const duplicate = fileRow("PROD.yaml", "x");
    const { errors, bundle } = validateBundle({ ...empty, files: [primary, first], assets: [duplicate] }, "inline", "compose.yaml");
    expect(bundle).toBeUndefined();
    expect(errors[`${primary.key}-path`]).toMatch(/primary Compose file/);
    expect(errors[`${first.key}-path`]).toBeUndefined();
    expect(errors[`${duplicate.key}-path`]).toMatch(/unique/);
  });

  it("rejects the Git compose path as an override and sends only paths for Git sources", () => {
    const clash = fileRow("deploy/compose.yml");
    expect(validateBundle({ ...empty, paths: [clash] }, "paths", "deploy/compose.yml").errors[`${clash.key}-path`]).toBeDefined();
    expect(validateBundle({ ...empty, paths: [fileRow("deploy/prod.yml")], profiles: "web" }, "paths", "deploy/compose.yml").bundle).toEqual({ files: [], assets: {}, paths: ["deploy/prod.yml"], profiles: ["web"] });
  });

  it("enforces profile names, limits, NUL and size", () => {
    expect(validateBundle({ ...empty, profiles: "-bad" }, "profiles", "compose.yaml").errors.profiles).toBeDefined();
    expect(validateBundle({ ...empty, profiles: "a a" }, "profiles", "compose.yaml").errors.profiles).toMatch(/once/);
    expect(validateBundle({ ...empty, profiles: Array.from({ length: 17 }, (_, i) => `p${i}`).join(" ") }, "profiles", "compose.yaml").errors.profiles).toMatch(/16/);
    const nul = fileRow("a.yaml", "a\0b");
    expect(validateBundle({ ...empty, files: [nul] }, "inline", "compose.yaml").errors[`${nul.key}-content`]).toMatch(/NUL/);
    const big = fileRow("a.yaml", "x".repeat(513 * 1024));
    expect(validateBundle({ ...empty, files: [big] }, "inline", "compose.yaml").errors.form).toMatch(/512 KiB/);
    expect(validateBundle(empty, "inline", "compose.yaml").errors.form).toMatch(/clear the selection/);
  });
});

it("moves rows within bounds and compares drafts by content", () => {
  expect(move([1, 2, 3], 2, -1)).toEqual([1, 3, 2]);
  expect(move([1, 2, 3], 0, -1)).toEqual([1, 2, 3]);
  const a = draftFromBundle({ files: [{ path: "a", content: "1" }], profiles: ["x"] });
  expect(sameDraft(a, draftFromBundle({ files: [{ path: "a", content: "1" }], profiles: ["x"] }))).toBe(true);
  expect(sameDraft(a, { ...a, profiles: "x," })).toBe(true);
  expect(sameDraft(a, { ...a, files: [...a.files].reverse().concat(fileRow("b", "")) })).toBe(false);
});
