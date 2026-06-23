// InstallScopeModal — the second modal opened from a row's Install button:
// "Where to install this extension?" with two choices, Globally (local:false)
// and To this project (local:true). Both call the daemon's `extension.install`
// RPC with the package name as the source. On success it surfaces the restart
// hint (workflows are picked up on the next project open — no hot-reload) and
// closes both modals.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { Modal } from "@/components/Modal";
import type { NpmExtension } from "@/types";

interface InstallScopeModalProps {
  cwd: string;
  ext: NpmExtension;
  /** Close just this scope modal (back to the browse list). */
  onClose: () => void;
  /** Close the whole browser (called after a successful install). */
  onInstalled: () => void;
}

export function InstallScopeModal({ cwd, ext, onClose, onInstalled }: InstallScopeModalProps) {
  const { effects } = useStore();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const install = async (local: boolean) => {
    setBusy(true);
    setErr(null);
    try {
      // The daemon installs by package name; an unversioned `npm:<name>` source
      // resolves to the latest published version.
      await ipc.extensionInstall(cwd, `npm:${ext.name}`, local);
      const scope = local ? "project" : "global";
      effects.setNotice({
        kind: "info",
        text: `Installed ${ext.name} (${scope}). Reopen the project for its workflow(s) to appear.`,
      });
      onInstalled();
    } catch (e) {
      setErr(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title="Where to install this extension?" onClose={onClose}>
      <p className="hint">
        Install <strong>{ext.name}</strong> globally (for every project on this machine) or only into
        the active project.
      </p>
      <div className="install-scope-actions">
        <button className="btn btn-primary" disabled={busy} onClick={() => void install(false)}>
          Globally
        </button>
        <button className="btn" disabled={busy} onClick={() => void install(true)}>
          To this project
        </button>
      </div>
      {busy && <div className="hint">Installing…</div>}
      {err && <div className="form-error">{err}</div>}
    </Modal>
  );
}
