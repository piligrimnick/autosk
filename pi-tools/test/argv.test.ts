/**
 * Pure unit tests for the `autosk` argv builders. These import only `argv.ts`
 * (which carries no pi runtime dependency), so they run with `bun test` without
 * installing the pi peer deps.
 */

import { describe, expect, test } from "bun:test";

import {
	buildCommentAddArgv,
	buildCommentListArgv,
	buildTaskCreateArgv,
	buildTaskListArgv,
	buildTaskUpdateArgv,
	requireId,
} from "../src/argv.ts";
import { AutoskCliError } from "../src/cli.ts";

describe("buildTaskCreateArgv", () => {
	test("title only", () => {
		expect(buildTaskCreateArgv({ title: "Build auth" })).toEqual(["create", "Build auth"]);
	});
	test("all flags in v2 order", () => {
		expect(
			buildTaskCreateArgv({
				title: "T",
				description: "D",
				blocks: ["ask-1", "ask-2"],
				blocked_by: ["ask-3"],
				workflow: "feature-dev",
			}),
		).toEqual([
			"create",
			"T",
			"--description",
			"D",
			"--blocks",
			"ask-1",
			"--blocks",
			"ask-2",
			"--blocked-by",
			"ask-3",
			"--workflow",
			"feature-dev",
		]);
	});
	test("rejects an empty/whitespace title", () => {
		expect(() => buildTaskCreateArgv({ title: "  " })).toThrow(AutoskCliError);
		expect(() => buildTaskCreateArgv({})).toThrow(/non-empty `title`/);
	});
});

describe("buildTaskUpdateArgv", () => {
	test("title and/or description", () => {
		expect(buildTaskUpdateArgv("ask-1", { title: "New" })).toEqual(["update", "ask-1", "--title", "New"]);
		expect(buildTaskUpdateArgv("ask-1", { description: "D" })).toEqual([
			"update",
			"ask-1",
			"--description",
			"D",
		]);
	});
	test("rejects an empty patch (v2 update is title/description only)", () => {
		expect(() => buildTaskUpdateArgv("ask-1", {})).toThrow(/at least one of: title, description/);
	});
});

describe("buildTaskListArgv", () => {
	test("no args → bare list", () => {
		expect(buildTaskListArgv({})).toEqual(["list"]);
	});
	test("statuses are comma-joined; blanks dropped; limit appended", () => {
		expect(buildTaskListArgv({ statuses: ["work", " human ", ""], limit: 5 })).toEqual([
			"list",
			"--status",
			"work,human",
			"--limit",
			"5",
		]);
	});
	test("limit 0 is omitted", () => {
		expect(buildTaskListArgv({ limit: 0 })).toEqual(["list"]);
	});
});

describe("buildCommentAddArgv / buildCommentListArgv", () => {
	test("add passes text positionally and an optional author flag", () => {
		expect(buildCommentAddArgv("ask-1", "note")).toEqual(["comment", "add", "ask-1", "note"]);
		expect(buildCommentAddArgv("ask-1", "note", "review")).toEqual([
			"comment",
			"add",
			"ask-1",
			"note",
			"--author",
			"review",
		]);
	});
	test("list", () => {
		expect(buildCommentListArgv("ask-1")).toEqual(["comment", "list", "ask-1"]);
	});
});

describe("requireId", () => {
	test("trims and returns a valid id", () => {
		expect(requireId("task", "show", "  ask-1 ")).toBe("ask-1");
	});
	test("rejects empty, naming the right field per domain", () => {
		expect(() => requireId("task", "show", "")).toThrow(/show requires `id`/);
		expect(() => requireId("comment", "list", "  ")).toThrow(/list requires `task_id`/);
	});
});
