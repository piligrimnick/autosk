// Root entry re-export.
//
// The compiled `autoskd` (`bun build --compile`) resolves an on-disk
// extension's bare `@autosk/*` imports with a minimal resolver that only honors
// a ROOT-LEVEL `index.ts`/`index.js` — it ignores `package.json` `exports` /
// `main` and any subdir target. So this package keeps its sources under `src/`
// but exposes a root `index.ts` the standalone daemon can find. (Interpreted
// Bun honors `exports` directly; this shim is what makes the published package
// loadable by the compiled binary.)
export * from "./src/index.ts";
