/**
 * The file store (plan §3.7(1-2)): task/comment/session persistence with atomic
 * writes, an mtime cache, and hybrid-ownership reconciliation.
 */

export { Store, type StoreOptions, type SchedulingRow } from "./store.ts";
export { TaskStore } from "./taskStore.ts";
export { SessionStore, type CreateSessionInput } from "./sessionStore.ts";
export { ProjectPaths, AUTOSK_DIR } from "./paths.ts";
export { systemClock, rfc3339Utc, type Clock } from "./clock.ts";
export { consoleLogger, CapturingLogger, type Logger } from "./logger.ts";
export { TasksWatcher, type WatcherOptions, type WatcherCallbacks } from "./watcher.ts";
export { KeyedMutex } from "./lock.ts";
export { atomicWrite, appendLine, statSig, fileSig, type AtomicWriteOptions } from "./atomic.ts";
export {
  type StoredTask,
  ENGINE_OWNED_TASK_FIELDS,
  serializeTask,
  parseTask,
  serializeComments,
  serializeCommentLine,
  parseComments,
  parseCommentsLenient,
  type ParsedComments,
  serializeSessionMeta,
  parseSessionMeta,
  serializeSessionHeader,
  serializeTranscriptLine,
  parseTranscript,
  parseTranscriptLenient,
  type ParsedTranscript,
} from "./records.ts";
export {
  STEP_VISITS_KEY,
  isEmptyMetadata,
  getStepVisits,
  bumpStepVisit,
  applyMetadataPatch,
  applyMetadataUnset,
} from "./metadata.ts";
