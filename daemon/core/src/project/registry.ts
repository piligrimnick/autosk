/**
 * The persisted project registry `~/.autosk/projects.json` (plan §3.7(1)).
 *
 * This is the daemon/GUI's durable list of known projects (the sidebar source
 * for `project.list/add/remove`). The CLI's walk-up from cwd (`resolve.ts`)
 * stays for ergonomics; this registry is the *explicit* list and the ONLY thing
 * that mutates it — reads never auto-register a project.
 *
 * Writes are atomic (temp + rename), the file is mode `0600` and its directory
 * `0700`, matching the daemon socket/token discipline. v2 drops `db_path`
 * (there is no database) — an entry is just `{ root, name }`.
 *
 * A missing/empty file is a fresh, empty registry; a NON-empty unparseable file
 * is a hard error rather than a silent reset, so the next `add` cannot clobber a
 * hand-edited / partially-written file (mirrors the v1 precedent).
 */

import { readFile } from "node:fs/promises";
import { basename, join } from "node:path";

import type { ProjectInfo } from "@autosk/sdk";
import { atomicWrite } from "../store/atomic.ts";
import { KeyedMutex } from "../store/lock.ts";

interface RegistryFile {
  projects: ProjectInfo[];
}

export class ProjectRegistry {
  private readonly path: string;
  private readonly lock = new KeyedMutex();

  constructor(path: string) {
    this.path = path;
  }

  /** The default location: `$HOME/.autosk/projects.json`. */
  static defaultPath(): string {
    const home = process.env.HOME;
    if (!home) throw new Error("HOME is not set");
    return join(home, ".autosk", "projects.json");
  }

  /** The registry at the default location. */
  static openDefault(): ProjectRegistry {
    return new ProjectRegistry(ProjectRegistry.defaultPath());
  }

  /** Every registered project, ordered by root. */
  async list(): Promise<ProjectInfo[]> {
    const file = await this.load();
    return [...file.projects].sort((a, b) => (a.root < b.root ? -1 : a.root > b.root ? 1 : 0));
  }

  /** Adds (or refreshes) a project keyed by canonical `root`. Idempotent. */
  async add(root: string, name?: string): Promise<ProjectInfo> {
    return this.lock.run("registry", async () => {
      const file = await this.load();
      const entry: ProjectInfo = { root, name: name && name.length > 0 ? name : basename(root) };
      file.projects = file.projects.filter((e) => e.root !== root);
      file.projects.push(entry);
      await this.save(file);
      return entry;
    });
  }

  /** Removes the project with the given canonical root. Returns whether it matched. */
  async remove(root: string): Promise<boolean> {
    return this.lock.run("registry", async () => {
      const file = await this.load();
      const before = file.projects.length;
      file.projects = file.projects.filter((e) => e.root !== root);
      const removed = file.projects.length !== before;
      if (removed) await this.save(file);
      return removed;
    });
  }

  private async load(): Promise<RegistryFile> {
    let bytes: string;
    try {
      bytes = await readFile(this.path, "utf8");
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code === "ENOENT") return { projects: [] };
      throw e;
    }
    if (bytes.trim().length === 0) return { projects: [] };
    let parsed: unknown;
    try {
      parsed = JSON.parse(bytes);
    } catch (e) {
      throw new Error(`${this.path} is corrupt (${e}); move it aside and retry`);
    }
    const rawProjects = (parsed as { projects?: unknown })?.projects;
    const projects: ProjectInfo[] = Array.isArray(rawProjects)
      ? rawProjects.flatMap((e) => {
          const root = (e as { root?: unknown }).root;
          if (typeof root !== "string" || root.length === 0) return [];
          const name = (e as { name?: unknown }).name;
          return [{ root, name: typeof name === "string" ? name : basename(root) }];
        })
      : [];
    return { projects };
  }

  private async save(file: RegistryFile): Promise<void> {
    const bytes = JSON.stringify({ projects: file.projects }, null, 2) + "\n";
    await atomicWrite(this.path, bytes, { mode: 0o600, dirMode: 0o700 });
  }
}
