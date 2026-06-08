// services/events.ts — the ONE place the frontend subscribes to Tauri events
// with `listen` (plan §6 "IPC discipline"). The Rust backend re-emits autoskd's
// JSON-RPC notifications verbatim (`app.emit("<same-name>", payload)`), so the
// React layer is oblivious to whether the daemon is local (UDS) or remote (TCP).
//
// Each event name gets a ref-counted hub: one underlying Tauri `listen` per
// event name fans out to N React subscribers, and the listener is torn down
// when the last subscriber unsubscribes (CodexMonitor pattern). Enforced as the
// sole `listen` site by scripts/check-ipc-discipline.mjs + eslint.

import { listen } from "@tauri-apps/api/event";
import type { DaemonStatus, JobEvent, TaskChangedEvent } from "@/types";

export type Unsubscribe = () => void;
type Listener<T> = (payload: T) => void;

interface SubscriptionOptions {
  onError?: (error: unknown) => void;
}

/** Build a ref-counted fan-out hub for a single Tauri event name. */
function createEventHub<T>(eventName: string) {
  const listeners = new Set<Listener<T>>();
  let unlisten: Unsubscribe | null = null;
  let listenPromise: Promise<Unsubscribe> | null = null;

  const start = (options?: SubscriptionOptions) => {
    if (unlisten || listenPromise) {
      return;
    }
    listenPromise = listen<T>(eventName, (event) => {
      for (const fn of listeners) {
        try {
          fn(event.payload);
        } catch (error) {
          console.error(`[events] ${eventName} listener failed`, error);
        }
      }
    });
    listenPromise
      .then((handler) => {
        listenPromise = null;
        if (listeners.size === 0) {
          // Everyone left before the listener attached — tear it down now.
          handler();
          return;
        }
        unlisten = handler;
      })
      .catch((error) => {
        listenPromise = null;
        options?.onError?.(error);
      });
  };

  const stop = () => {
    if (unlisten) {
      try {
        unlisten();
      } catch {
        /* ignore double-unlisten during teardown */
      }
      unlisten = null;
    }
  };

  const subscribe = (onEvent: Listener<T>, options?: SubscriptionOptions): Unsubscribe => {
    listeners.add(onEvent);
    start(options);
    return () => {
      listeners.delete(onEvent);
      if (listeners.size === 0) {
        stop();
      }
    };
  };

  return { subscribe };
}

// One hub per autoskd notification re-emitted by the Rust backend.
const jobEventHub = createEventHub<JobEvent>("job-event");
const taskChangedHub = createEventHub<TaskChangedEvent>("task-changed");
const projectChangedHub = createEventHub<Record<string, never>>("project-changed");
const daemonStatusHub = createEventHub<DaemonStatus>("daemon-status");

/** Live transcript / status / done / error frames for any running job. */
export function subscribeJobEvents(
  onEvent: (event: JobEvent) => void,
  options?: SubscriptionOptions,
): Unsubscribe {
  return jobEventHub.subscribe(onEvent, options);
}

/** A project's task/job state changed — re-fetch the affected project. */
export function subscribeTaskChanged(
  onEvent: (event: TaskChangedEvent) => void,
  options?: SubscriptionOptions,
): Unsubscribe {
  return taskChangedHub.subscribe(onEvent, options);
}

/** The project registry or a workflow/agent list changed — re-fetch. */
export function subscribeProjectChanged(
  onEvent: () => void,
  options?: SubscriptionOptions,
): Unsubscribe {
  return projectChangedHub.subscribe(() => onEvent(), options);
}

/** The Tauri backend's daemon connection state changed (connect/disconnect). */
export function subscribeDaemonStatus(
  onEvent: (status: DaemonStatus) => void,
  options?: SubscriptionOptions,
): Unsubscribe {
  return daemonStatusHub.subscribe(onEvent, options);
}
