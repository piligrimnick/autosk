/**
 * `.autosk/` on-disk layout (plan §3.1, §3.2, §3.7(1)).
 *
 * A project root is the parent of its `.autosk/` directory. Everything the
 * file store reads/writes hangs off these helpers so the layout lives in one
 * place:
 *
 *   <root>/.autosk/
 *     tasks/<id>/task.json
 *     tasks/<id>/comments.jsonl
 *     sessions/<session-id>.json
 *     sessions/<session-id>.jsonl
 *     extensions/                 (populated by the loader in P3)
 */

import { join } from "node:path";

/** The per-project directory name (unchanged from v1). */
export const AUTOSK_DIR = ".autosk";

/** Absolute-path helpers for one project's `.autosk/` tree. */
export class ProjectPaths {
  /** Canonical project root (parent of `.autosk/`). */
  readonly root: string;

  constructor(root: string) {
    this.root = root;
  }

  /** `<root>/.autosk`. */
  get autoskDir(): string {
    return join(this.root, AUTOSK_DIR);
  }

  /** `<root>/.autosk/tasks`. */
  get tasksDir(): string {
    return join(this.autoskDir, "tasks");
  }

  /** `<root>/.autosk/sessions`. */
  get sessionsDir(): string {
    return join(this.autoskDir, "sessions");
  }

  /** `<root>/.autosk/extensions`. */
  get extensionsDir(): string {
    return join(this.autoskDir, "extensions");
  }

  /** `<root>/.autosk/tasks/<id>`. */
  taskDir(id: string): string {
    return join(this.tasksDir, id);
  }

  /** `<root>/.autosk/tasks/<id>/task.json`. */
  taskJson(id: string): string {
    return join(this.tasksDir, id, "task.json");
  }

  /** `<root>/.autosk/tasks/<id>/comments.jsonl`. */
  commentsJsonl(id: string): string {
    return join(this.tasksDir, id, "comments.jsonl");
  }

  /** `<root>/.autosk/sessions/<session-id>.json`. */
  sessionMeta(id: string): string {
    return join(this.sessionsDir, `${id}.json`);
  }

  /** `<root>/.autosk/sessions/<session-id>.jsonl`. */
  sessionTranscript(id: string): string {
    return join(this.sessionsDir, `${id}.jsonl`);
  }

  /** The directories `project.init` materialises (plan §3.7(1)). */
  skeletonDirs(): string[] {
    return [this.autoskDir, this.tasksDir, this.sessionsDir, this.extensionsDir];
  }
}
