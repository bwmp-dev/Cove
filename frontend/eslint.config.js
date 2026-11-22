import js from "@eslint/js";
import jsxA11y from "eslint-plugin-jsx-a11y";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist", "node_modules"] },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [js.configs.recommended, ...tseslint.configs.recommendedTypeChecked, jsxA11y.flatConfigs.recommended],
    languageOptions: {
      globals: globals.browser,
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    plugins: { "react-hooks": reactHooks },
    settings: { "jsx-a11y": { components: { Checkbox: "input" } } },
    rules: {
      ...reactHooks.configs.recommended.rules,
      // Scrollable log regions must be keyboard focusable (WCAG 2.1.1).
      "jsx-a11y/no-noninteractive-tabindex": ["error", { roles: ["tabpanel", "region"] }],
      "@typescript-eslint/only-throw-error": ["error", { allow: [{ from: "package", package: "@tanstack/router-core", name: "Redirect" }] }],
    },
  },
);
