import { describe, expect, it } from "vitest";
import { formatCompose } from "./compose-format";

describe("formatCompose", () => {
  it("expands JSON-style Compose into block YAML", () => {
    const source = '{"services": {"web": {"image": "nginx:1.27", "ports": ["80:80"], "volumes": ["data:/data"]}}, "volumes": {"data": {"name": "cove_data", "external": true}}}';
    expect(formatCompose(source)).toEqual({
      text: [
        "services:",
        "  web:",
        "    image: nginx:1.27",
        "    ports:",
        '      - "80:80"',
        "    volumes:",
        "      - data:/data",
        "volumes:",
        "  data:",
        "    name: cove_data",
        "    external: true",
        "",
      ].join("\n"),
    });
  });

  it("normalises indentation and keeps comments", () => {
    const source = "# stack\nservices:\n    web:\n        image: nginx   # pinned later\n        command: [sh, -c, 'echo hi']\n";
    expect(formatCompose(source).text).toBe("# stack\nservices:\n  web:\n    image: nginx # pinned later\n    command:\n      - sh\n      - -c\n      - echo hi\n");
  });

  it("keeps quotes that change how YAML 1.1 or 1.2 reads a value", () => {
    const source = 'x:\n  enabled: "true"\n  port: "8080"\n  legacy: "on"\n  mapping: "22:22"\n  spaced: " a"\n  comment: "a #b"\n  empty: ""\n  "y": 1\n';
    expect(formatCompose(source).text).toBe(source);
  });

  it("does not fold long values", () => {
    const command = "a ".repeat(100).trim();
    expect(formatCompose(`command: ${command}`).text).toBe(`command: ${command}\n`);
  });

  it("reports the parse error instead of formatting", () => {
    const result = formatCompose("services:\n  web: [nginx\n");
    expect(result.text).toBeUndefined();
    expect(result.error).toMatch(/line \d+, column \d+$/);
  });
});
