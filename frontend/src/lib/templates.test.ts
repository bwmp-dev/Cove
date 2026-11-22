import { describe, expect, it } from "vitest";
import { templates, templateDraft, templateValues } from "./templates";

describe("resource templates", () => {
  it.each(templates)("prepares $name with independent secrets and valid placement defaults", (template) => {
    const first = templateValues(template);
    const second = templateValues(template);
    for (const variable of template.variables.filter((item) => item.secret)) {
      expect(first[variable.key]).toMatch(/^[a-f0-9]{64}$/);
      expect(first[variable.key]).not.toEqual(second[variable.key]);
    }
    const draft = templateDraft(template, first, "monitor.example.com");
    expect(draft.compose).toContain("volumes:");
    expect(draft.compose).not.toContain("container_name:");
    expect(draft.compose).not.toContain("ports:");
    expect(draft.routes).toEqual(template.web ? [{ domain: "monitor.example.com", service: template.web.service, port: template.web.port }] : []);
    if (template.web?.urlVariable) expect(draft.environment[template.web.urlVariable]).toBe("https://monitor.example.com");
    for (const match of draft.compose.matchAll(/\$\{([A-Z_]+):\?/g)) { if (!match[1]) throw new Error("Missing variable name"); expect(draft.environment[match[1]]).toBeTruthy(); }
  });
  it("rejects malformed web domains and unsafe URL credentials", () => {
    const beszel = templates.find((item) => item.id === "beszel")!;
    for (const domain of ["", "https://example.com", "example.com/path", "example.com:8090", "*.example.com", "-bad.example.com"]) {
      expect(() => templateDraft(beszel, {}, domain)).toThrow("hostname");
    }
    const umami = templates.find((item) => item.id === "umami")!;
    expect(() => templateDraft(umami, { ...templateValues(umami), DATABASE_PASSWORD: "password@db/other" }, "stats.example.com")).toThrow("hexadecimal");
  });
});
