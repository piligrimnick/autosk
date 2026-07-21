/**
 * The GitHub target a `gh-review` task points at, parsed from the task title
 * (or description): a `https://github.com/<owner>/<repo>/issues/<n>` or
 * `…/pull/<n>` URL.
 */
export interface GhTarget {
  /** `issue` for an `/issues/<n>` URL, `pull` for a `/pull/<n>` URL. */
  kind: "issue" | "pull";
  owner: string;
  repo: string;
  number: number;
  /** The canonical URL: `https://github.com/<owner>/<repo>/(issues|pull)/<n>`. */
  url: string;
}

// Matches the first GitHub issue/PR URL embedded anywhere in the text (leading
// `www.` and any path casing tolerated; stops at the issue/PR number, so a
// trailing `/files`, `?q=1`, or sentence punctuation does not matter).
const GH_URL_RE = /https:\/\/(?:www\.)?github\.com\/([^/\s]+)\/([^/\s]+)\/(issues|pull)\/(\d+)/i;

/**
 * Extracts the first GitHub issue/PR URL from `text` (a task title or
 * description). Returns `undefined` when there is none — the caller decides
 * what that means (the workflow's `onTransit` rejects the enroll).
 */
export function parseGhTarget(text: string): GhTarget | undefined {
  const m = GH_URL_RE.exec(text);
  if (!m) return undefined;
  // A successful match always carries all four capture groups (regex above).
  const owner = m[1]!;
  const repo = m[2]!;
  const kind: GhTarget["kind"] = m[3]!.toLowerCase() === "pull" ? "pull" : "issue";
  const number = Number.parseInt(m[4]!, 10);
  return {
    kind,
    owner,
    repo,
    number,
    url: `https://github.com/${owner}/${repo}/${kind === "pull" ? "pull" : "issues"}/${number}`,
  };
}
