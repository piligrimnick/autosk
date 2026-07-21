// ExtensionRow — one npm `autosk-extension` package in the browse modal. The
// main info area is a button that opens the package's npmjs.com page in the
// default browser (via the opener plugin); the Install button sits beside it as
// a SIBLING (not nested), so mouse and keyboard activation never cross wires.
// When the package is already installed, an "Installed (scope)" badge shows and
// the Install button is disabled.

import type { NpmExtension } from "@/types";
import { openExternal } from "@/services/opener";
import { localDate } from "@/components/common";
import { formatDownloads, type InstalledScope } from "../extensions";

interface ExtensionRowProps {
  ext: NpmExtension;
  installedScope: InstalledScope | null;
  onInstall: (ext: NpmExtension) => void;
}

export function ExtensionRow({ ext, installedScope, onInstall }: ExtensionRowProps) {
  const installed = installedScope !== null;
  const updated = localDate(ext.updated);

  return (
    <li className="ext-row">
      <button
        type="button"
        className="ext-main"
        title={`Open ${ext.name} on npmjs.com`}
        onClick={() =>
          // openExternal now rejects for non-HTTP(S) URLs; catch so a malformed
          // npm_url cannot surface as an unhandled promise rejection.
          void openExternal(ext.npm_url).catch((error) => console.error(error))
        }
      >
        <span className="ext-head">
          <span className="ext-name">{ext.name}</span>
          {ext.version && <span className="ext-version">v{ext.version}</span>}
        </span>
        {ext.description && <span className="ext-desc">{ext.description}</span>}
        <span className="ext-meta">
          <span className="ext-downloads">{formatDownloads(ext.weekly_downloads)} weekly</span>
          {ext.publisher && <span className="ext-publisher">{ext.publisher}</span>}
          {updated && <span className="ext-updated">updated {updated}</span>}
        </span>
      </button>
      <div className="ext-actions">
        {installed ? (
          <span className="ext-badge" title={`Already installed (${installedScope})`}>
            Installed ({installedScope})
          </span>
        ) : null}
        <button
          className="btn btn-sm btn-primary"
          disabled={installed}
          onClick={() => {
            if (!installed) onInstall(ext);
          }}
        >
          Install
        </button>
      </div>
    </li>
  );
}
