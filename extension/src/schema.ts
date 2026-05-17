import { Type, type Static } from "typebox";

/**
 * Single tool surface: `autosk { action, args }`.
 *
 * `action` is constrained to a known set so the LLM cannot smuggle in
 * arbitrary subcommands (no `sql`, no `migrate`, no `completion`). `args` is
 * a permissive bag; each action ignores keys it does not need.
 */
export const AutoskActionSchema = Type.Union([
	Type.Literal("create"),
	Type.Literal("show"),
	Type.Literal("update"),
	Type.Literal("done"),
	Type.Literal("cancel"),
	Type.Literal("reopen"),
	Type.Literal("block"),
	Type.Literal("unblock"),
	Type.Literal("dep_list"),
	Type.Literal("list"),
	Type.Literal("ready"),
	Type.Literal("next"),
	Type.Literal("init"),
	Type.Literal("step_next"),
]);

export const AutoskTaskStatusSchema = Type.Union([
	Type.Literal("new"),
	Type.Literal("in_workflow"),
	Type.Literal("human_feedback"),
	Type.Literal("done"),
	Type.Literal("cancelled"),
]);

export const AutoskArgsSchema = Type.Partial(
	Type.Object({
		/** Target task id, e.g. "as-a1b2". Required for show/update/claim/done/cancel/reopen/block/unblock/dep_list. */
		id: Type.String({ description: "Target task id (e.g. as-a1b2)." }),
		/** Blocker task ids — used by block / unblock. */
		blocker_ids: Type.Array(Type.String(), {
			description: "Blocker task ids. For block: each id will block `id`. For unblock: each edge removed.",
		}),
		/** Remove every blocker edge for `id`. unblock only. */
		all: Type.Boolean({ description: "unblock only: remove every blocker edge for `id`." }),
		/** Task title. create / update. */
		title: Type.String({ description: "Task title. Used by create and update." }),
		/** Task description. create / update. */
		description: Type.String({ description: "Task description. Used by create and update." }),
		/** Priority 0..3 (0=highest). create / update. */
		priority: Type.Integer({
			minimum: 0,
			maximum: 3,
			description: "Priority 0..3 (0 = highest). Used by create and update.",
		}),
		/** Target status. update only — any → any. */
		status: AutoskTaskStatusSchema,
		/** create only: ids this new task will block. */
		blocks: Type.Array(Type.String(), {
			description: "create only: this new task will be a blocker for each of these ids.",
		}),
		/** create only: ids that block this new task. */
		blocked_by: Type.Array(Type.String(), {
			description: "create only: each of these ids will block this new task.",
		}),
		/** ready / list: max rows (0 = unlimited). */
		limit: Type.Integer({
			minimum: 0,
			description: "ready/list: max rows to return. 0 = unlimited. Defaults: list none, ready none.",
		}),
		/** list: comma-separated status filter, or "all". Default in CLI: new,claimed. */
		status_filter: Type.String({
			description: "list only: comma-separated statuses to include, or 'all'. Default: new,claimed.",
		}),
		/** list: filter by exact priority. */
		priority_filter: Type.Integer({
			minimum: 0,
			maximum: 3,
			description: "list only: filter by exact priority (0..3).",
		}),
		/** init: reserved prefix override (currently always 'as'). */
		prefix: Type.String({ description: "init only: reserved ID prefix (currently always 'as')." }),
		/** step_next: transition target — sibling step name OR done|cancelled|human_feedback. */
		to: Type.String({
			description: "step_next only: transition target. Either a sibling step name in the current workflow, or one of {done, cancelled, human_feedback}.",
		}),
	}),
);

export const AutoskParamsSchema = Type.Object({
	action: AutoskActionSchema,
	args: Type.Optional(AutoskArgsSchema),
});

export type AutoskArgs = Static<typeof AutoskArgsSchema>;
export type AutoskParams = Static<typeof AutoskParamsSchema>;
