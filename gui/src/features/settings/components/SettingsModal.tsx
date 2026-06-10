// SettingsModal — backend transport mode in a titlebar-launched modal (redesign
// plan §8.7, decision #7). Ported from the legacy SettingsView body; Local =
// autoskd over UDS (auto-spawned), Remote = host:port + token over TCP.

import { useEffect, useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import type { AppSettings, BackendMode } from "@/types";
import { Modal } from "@/components/Modal";
import { Section } from "@/components/common";
import { isWebviewZoomSupported } from "@/features/layout/utils/platform";
import {
  UI_SCALE_MAX,
  UI_SCALE_MIN,
  UI_SCALE_STEP,
  formatUiScale,
} from "@/features/layout/utils/uiScale";

export function SettingsModal({ onClose }: { onClose: () => void }) {
  const { state, effects } = useStore();
  const [draft, setDraft] = useState<AppSettings>(
    state.settings ?? { backend_mode: "local", remote_host: "", remote_token: "" },
  );
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);

  // UI zoom is desktop-only (Tauri setZoom). While dragging the slider we keep a
  // local "draft" value for smooth thumb + label feedback, but only commit it
  // to the store (which calls setZoom) on release — not on every input frame.
  const zoomSupported = isWebviewZoomSupported();
  const [zoomDraft, setZoomDraft] = useState<number | null>(null);
  const zoomValue = zoomDraft ?? state.ui.uiScale;
  const commitZoom = (e: { currentTarget: HTMLInputElement }) => {
    effects.setUiScale(Number(e.currentTarget.value));
    setZoomDraft(null);
  };

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
    <Modal
      title="Settings"
      onClose={onClose}
      footer={
        <>
          <button className="btn btn-primary" disabled={busy} onClick={() => void save()}>
            Save & reconnect
          </button>
          <button className="btn" disabled={busy} onClick={() => void effects.reconnect()}>
            Force reconnect
          </button>
          {saved && <span className="saved-tag">saved ✓</span>}
        </>
      }
    >
      {zoomSupported && (
        <Section title="Appearance">
          <label className="field">
            <span className="field-label">UI zoom · {formatUiScale(zoomValue)}</span>
            <div className="zoom-row">
              <input
                type="range"
                className="zoom-slider"
                min={UI_SCALE_MIN}
                max={UI_SCALE_MAX}
                step={UI_SCALE_STEP}
                value={zoomValue}
                onChange={(e) => setZoomDraft(Number(e.currentTarget.value))}
                onPointerUp={commitZoom}
                onKeyUp={commitZoom}
                onBlur={commitZoom}
              />
              <button
                className="btn btn-sm"
                disabled={zoomValue === 1}
                onClick={() => effects.setUiScale(1)}
              >
                Reset
              </button>
            </div>
          </label>
          <p className="hint">
            Scales the whole window. Keyboard: <code>Cmd</code>/<code>Ctrl</code> + <code>+</code> / <code>-</code> /{" "}
            <code>0</code>.
          </p>
        </Section>
      )}

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
      </Section>
    </Modal>
  );
}
