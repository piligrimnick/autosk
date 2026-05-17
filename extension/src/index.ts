import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { registerAutoskTool } from "./tool.ts";

/**
 * pi extension entry point. Exposes the `autosk` tool that maps a small,
 * fixed action enum onto the local `autosk` CLI binary.
 */
export default function autoskExtension(pi: ExtensionAPI) {
	registerAutoskTool(pi);
}
