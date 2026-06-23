/**
 * ESLint config for the autosk Tauri GUI.
 *
 * The `no-restricted-imports` rule enforces the IPC discipline at lint time
 * (plan §6): only `src/services/ipc.ts` may import Tauri `invoke`, and only
 * `src/services/events.ts` may import `listen`. The
 * `scripts/check-ipc-discipline.mjs` guard backstops this in `npm run
 * typecheck` for environments without eslint.
 */
module.exports = {
  root: true,
  env: { browser: true, es2021: true, node: true },
  parser: "@typescript-eslint/parser",
  parserOptions: { ecmaVersion: 2021, sourceType: "module", ecmaFeatures: { jsx: true } },
  plugins: ["@typescript-eslint"],
  extends: ["eslint:recommended", "plugin:@typescript-eslint/recommended"],
  ignorePatterns: ["dist", "src-tauri", "node_modules", "*.cjs"],
  rules: {
    "@typescript-eslint/no-explicit-any": "off",
    "@typescript-eslint/no-unused-vars": ["warn", { argsIgnorePattern: "^_" }],
    "no-restricted-imports": [
      "error",
      {
        paths: [
          {
            name: "@tauri-apps/api/core",
            importNames: ["invoke"],
            message: "Use the typed shims in src/services/ipc.ts (the only invoke site).",
          },
          {
            name: "@tauri-apps/api/event",
            importNames: ["listen"],
            message: "Use the hub in src/services/events.ts (the only listen site).",
          },
        ],
      },
    ],
  },
  overrides: [
    {
      files: ["src/services/ipc.ts"],
      rules: {
        "no-restricted-imports": [
          "error",
          {
            paths: [
              {
                name: "@tauri-apps/api/event",
                importNames: ["listen"],
                message: "listen belongs in src/services/events.ts.",
              },
            ],
          },
        ],
      },
    },
    {
      files: ["src/services/events.ts"],
      rules: {
        "no-restricted-imports": [
          "error",
          {
            paths: [
              {
                name: "@tauri-apps/api/core",
                importNames: ["invoke"],
                message: "invoke belongs in src/services/ipc.ts.",
              },
            ],
          },
        ],
      },
    },
  ],
};
