// SettingsView — backend transport mode (plan §6: "mode is an app setting").
// Local = autoskd sidecar over UDS (auto-spawned); Remote = a configured
// host:port + token over TCP. The frontend is identical for both — only this
// setting (and the Rust command's switch) changes.

import { useEffect, useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import type { AppSettings, BackendMode } from "@/types";
import { Section } from "./common";

export function SettingsView() {
  const { state, effects } = useStore();
  const [draft, setDraft] = useState<AppSettings>(
    state.settings ?? { backend_mode: "local", remote_host: "", remote_token: "" },
  );
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    if (state.settings) setDraft(state.settings);
  }, [state.settings]);

  const save = async () => {
    setBusy(true);
    setSaved(false);
    try {
      const next = await ipc.updateAppSettings(draft);
      // updateAppSettings reconnects on the backend; pull the new status.
      await effects.reconnect();
      setDraft(next);
      setSaved(true);
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    } finally {
      setBusy(false);
    }
  };

  const setMode = (mode: BackendMode) => setDraft({ ...draft, backend_mode: mode });

  return (
    <div className="view">
      <div className="view-head">
        <h2>Settings</h2>
      </div>

      <Section title="Backend mode">
        <div className="seg">
          <button
            className={`seg-btn ${draft.backend_mode === "local" ? "seg-active" : ""}`}
            onClick={() => setMode("local")}
          >
            Local (sidecar / UDS)
          </button>
          <button
            className={`seg-btn ${draft.backend_mode === "remote" ? "seg-active" : ""}`}
            onClick={() => setMode("remote")}
          >
            Remote (TCP + token)
          </button>
        </div>
        <p className="hint">
          Local connects to a co-located <code>autoskd</code> over a Unix socket, auto-spawning it if absent. Remote
          dials a host running <code>autoskd --tcp HOST:PORT</code> with token auth.
        </p>
      </Section>

      {draft.backend_mode === "remote" && (
        <Section title="Remote daemon">
          <label className="field">
            <span className="field-label">Host:port</span>
            <input
              className="input"
              value={draft.remote_host}
              placeholder="127.0.0.1:7077"
              onChange={(e) => setDraft({ ...draft, remote_host: e.target.value })}
            />
          </label>
          <label className="field">
            <span className="field-label">Token</span>
            <input
              className="input"
              type="password"
              value={draft.remote_token}
              placeholder="contents of ~/.autosk/daemon-token on the host"
              onChange={(e) => setDraft({ ...draft, remote_token: e.target.value })}
            />
          </label>
        </Section>
      )}

      <Section title="Connection">
        <div className="conn-line">
          <span className={`conn-dot ${state.daemon.connected ? "conn-ok" : "conn-down"}`} />
          {state.daemon.connected ? "connected" : "disconnected"} · {state.daemon.mode}
          {state.daemon.error ? ` · ${state.daemon.error}` : ""}
        </div>
        <div className="view-actions">
          <button className="btn btn-primary" disabled={busy} onClick={() => void save()}>
            Save & reconnect
          </button>
          <button className="btn" disabled={busy} onClick={() => void effects.reconnect()}>
            Force reconnect
          </button>
          {saved && <span className="saved-tag">saved ✓</span>}
        </div>
      </Section>
    </div>
  );
}
