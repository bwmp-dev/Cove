import { commandProblem, composeServices, timeoutProblem } from "./exec";

describe("commandProblem", () => {
  it("accepts a program with arguments", () => {
    expect(commandProblem(["ls", "-la", "/data"])).toBeNull();
  });

  it("requires a program", () => {
    expect(commandProblem([])).toMatch(/program/);
    expect(commandProblem(["", "x"])).toMatch(/program/);
  });

  it("enforces argument count and size limits", () => {
    expect(commandProblem(Array.from({ length: 65 }, () => "a"))).toMatch(/64 arguments/);
    expect(commandProblem(["echo", "x".repeat(4097)])).toMatch(/Argument 1 /);
    expect(commandProblem(["echo", ...Array.from({ length: 5 }, () => "x".repeat(4000))])).toMatch(/16 KiB/);
  });

  it("counts multi-byte characters as bytes", () => {
    expect(commandProblem(["echo", "é".repeat(2049)])).toMatch(/4096 bytes/);
  });

  it("rejects NUL characters", () => {
    expect(commandProblem(["echo", "a\0b"])).toMatch(/NUL/);
  });
});

describe("timeoutProblem", () => {
  it("allows 1 to 30 whole seconds", () => {
    expect(timeoutProblem(1)).toBeNull();
    expect(timeoutProblem(30)).toBeNull();
    expect(timeoutProblem(0)).not.toBeNull();
    expect(timeoutProblem(31)).not.toBeNull();
    expect(timeoutProblem(2.5)).not.toBeNull();
    expect(timeoutProblem(Number.NaN)).not.toBeNull();
  });
});

describe("composeServices", () => {
  it("lists the Compose service names", () => {
    expect(composeServices("services:\n  web:\n    image: nginx\n  worker:\n    image: app\n")).toEqual(["web", "worker"]);
  });

  it("returns nothing for files without services or invalid YAML", () => {
    expect(composeServices("version: '3'\n")).toEqual([]);
    expect(composeServices("services: [")).toEqual([]);
  });
});
