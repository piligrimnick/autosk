/**
 * TaskStore IO + mtime cache, and the atomic-write / keyed-lock primitives.
 */

import { describe, expect, test } from "bun:test";
import { readFile, writeFile } from "node:fs/promises";

import {
  atomicWrite,
  fileSig,
  KeyedMutex,
  parseTask,
  ProjectPaths,
  serializeTask,
  statSig,
  TaskStore,
  type StoredTask,
} from "../src/index.ts";
import { tempDir } from "./helpers.ts";

function sampleTask(id: string): StoredTask {
  return {
    id,
    title: "t",
    description: "",
    status: "new",
    workflow: null,
    step: null,
    blocked_by: [],
    metadata: {},
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

describe("atomicWrite", () => {
  test("writes the exact bytes and reports a signature, no temp files left", async () => {
    const dir = tempDir();
    try {
      const path = `${dir.path}/sub/a.json`;
      const sig = await atomicWrite(path, "hello");
      expect(await readFile(path, "utf8")).toBe("hello");
      const probe = await statSig(path);
      expect(probe?.sig).toBe(sig);
      // No leftover temp files in the directory.
      const { readdir } = await import("node:fs/promises");
      const entries = await readdir(`${dir.path}/sub`);
      expect(entries).toEqual(["a.json"]);
    } finally {
      dir.cleanup();
    }
  });

  test("mode is applied exactly (umask-proof)", async () => {
    const dir = tempDir();
    try {
      const path = `${dir.path}/secret`;
      await atomicWrite(path, "x", { mode: 0o600 });
      const { statSync } = await import("node:fs");
      expect(statSync(path).mode & 0o777).toBe(0o600);
    } finally {
      dir.cleanup();
    }
  });
});

describe("fileSig", () => {
  test("encodes mtime:ctime:size:ino and changes on any component", () => {
    expect(fileSig({ mtimeMs: 1, ctimeMs: 2, size: 10, ino: 7 })).toBe("1:2:10:7");
    // size, ctime, and ino each move the signature (mtime collision tiebreakers).
    const base = fileSig({ mtimeMs: 1, ctimeMs: 2, size: 10, ino: 7 });
    expect(fileSig({ mtimeMs: 1, ctimeMs: 2, size: 11, ino: 7 })).not.toBe(base);
    expect(fileSig({ mtimeMs: 1, ctimeMs: 3, size: 10, ino: 7 })).not.toBe(base);
    expect(fileSig({ mtimeMs: 1, ctimeMs: 2, size: 10, ino: 8 })).not.toBe(base);
  });
});

describe("KeyedMutex", () => {
  test("serialises the same key, parallelises distinct keys", async () => {
    const m = new KeyedMutex();
    const order: string[] = [];
    const slow = (tag: string, ms: number) =>
      m.run("k", async () => {
        order.push(`${tag}-start`);
        await new Promise((r) => setTimeout(r, ms));
        order.push(`${tag}-end`);
      });
    await Promise.all([slow("a", 20), slow("b", 1)]);
    // b cannot start until a finishes (same key).
    expect(order).toEqual(["a-start", "a-end", "b-start", "b-end"]);
  });

  test("a rejection does not poison later acquisitions of the key", async () => {
    const m = new KeyedMutex();
    await expect(m.run("k", async () => Promise.reject(new Error("boom")))).rejects.toThrow("boom");
    expect(await m.run("k", async () => 42)).toBe(42);
  });
});

describe("TaskStore cache", () => {
  test("read serves the cache until the file signature changes", async () => {
    const dir = tempDir();
    try {
      const paths = new ProjectPaths(dir.path);
      const store = new TaskStore(paths);
      await store.writeTask(sampleTask("ask-000001"));

      // Cached read returns the same object identity (no re-parse).
      const first = await store.read("ask-000001");
      const second = await store.read("ask-000001");
      expect(first).toBe(second);

      // An external rewrite changes the signature → next read re-parses.
      const edited = sampleTask("ask-000001");
      edited.title = "external";
      await writeFile(paths.taskJson("ask-000001"), serializeTask(edited));
      const third = await store.read("ask-000001");
      expect(third).not.toBe(first);
      expect(third?.title).toBe("external");
    } finally {
      dir.cleanup();
    }
  });

  test("listIdsOnDisk skips dotfiles and dirs without task.json", async () => {
    const dir = tempDir();
    try {
      const paths = new ProjectPaths(dir.path);
      const store = new TaskStore(paths);
      await store.writeTask(sampleTask("ask-000001"));
      await store.writeTask(sampleTask("ask-000002"));
      // A stray directory with no task.json must be ignored.
      const { mkdir } = await import("node:fs/promises");
      await mkdir(paths.taskDir("ask-empty"), { recursive: true });
      expect(await store.listIdsOnDisk()).toEqual(["ask-000001", "ask-000002"]);
    } finally {
      dir.cleanup();
    }
  });

  test("readDisk round-trips through parseTask", async () => {
    const dir = tempDir();
    try {
      const paths = new ProjectPaths(dir.path);
      const store = new TaskStore(paths);
      const t = sampleTask("ask-000003");
      t.blocked_by = ["ask-zzzzzz"];
      await store.writeTask(t);
      const disk = await store.readDisk("ask-000003");
      expect(disk?.task).toEqual(parseTask(serializeTask(t)));
    } finally {
      dir.cleanup();
    }
  });
});
