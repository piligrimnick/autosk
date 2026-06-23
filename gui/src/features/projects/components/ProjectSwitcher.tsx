// ProjectSwitcher — the titlebar (status bar) dropdown, to the right of the
// macOS traffic lights (redesign plan §8.2, decision #7). Shows the active
// project name; the menu switches project, removes one from the registry, and
// opens the add/init modal.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { useConfirm } from "@/components/ConfirmDialog";
import { Menu, MenuDivider, MenuItem, MenuLabel } from "@/features/shared/Menu";
import { AddProjectModal } from "./AddProjectModal";

export function ProjectSwitcher() {
  const { state, effects } = useStore();
  const confirm = useConfirm();
  const [anchor, setAnchor] = useState<DOMRect | null>(null);
  const [adding, setAdding] = useState(false);
  const activeName = state.projects.find((p) => p.root === state.activeProject)?.name ?? "No project";
  // Extension load errors for the active project (project.diagnostics).
  const extErrors = state.activeProject
    ? state.byProject[state.activeProject]?.diagnostics?.extensions ?? []
    : [];
  const close = () => setAnchor(null);

  const remove = async (root: string, name: string) => {
    close();
    const ok = await confirm({
      title: "Remove project",
      message: `Remove ${name} from the registry?\n\nThe project's .autosk/ directory is left untouched.`,
      confirmLabel: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      await ipc.projectRemove(root);
      await effects.refreshProjects();
    } catch (err) {
      effects.setNotice({ kind: "error", text: String((err as Error).message ?? err) });
    }
  };

  return (
    <>
      <button
        className="project-switcher"
        title="Switch project"
        onClick={(e) => setAnchor(e.currentTarget.getBoundingClientRect())}
      >
        <span className="project-switcher-name">{activeName}</span>
        {extErrors.length > 0 && (
          <span
            className="project-switcher-warn"
            title={`${extErrors.length} extension load error(s)`}
          >
            ⚠
          </span>
        )}
        <span className="project-switcher-caret">▾</span>
      </button>

      <Menu open={anchor !== null} anchor={anchor} onClose={close} align="left">
        <MenuLabel>Projects</MenuLabel>
        {state.projects.length === 0 ? (
          <MenuItem onClick={() => { close(); setAdding(true); }}>Add a project…</MenuItem>
        ) : (
          state.projects.map((p) => (
            <div className="menu-row" key={p.root}>
              <button
                className={`menu-item menu-item-grow${p.root === state.activeProject ? " menu-item-active" : ""}`}
                title={p.root}
                onClick={() => { close(); void effects.selectProject(p.root); }}
              >
                {p.root === state.activeProject ? "● " : ""}
                {p.name}
              </button>
              <button className="menu-remove" title="Remove from registry" onClick={() => void remove(p.root, p.name)}>
                ×
              </button>
            </div>
          ))
        )}
        {extErrors.length > 0 && (
          <>
            <MenuDivider />
            <MenuLabel>Extension load errors</MenuLabel>
            {extErrors.map((e, i) => (
              <div className="menu-diagnostic" key={`${e.source}:${i}`} title={e.error}>
                <span className="menu-diagnostic-source">{e.source}</span>
                <span className="menu-diagnostic-error">{e.error}</span>
              </div>
            ))}
          </>
        )}
        <MenuDivider />
        <MenuItem onClick={() => { close(); setAdding(true); }}>Add / init project…</MenuItem>
      </Menu>

      {adding && <AddProjectModal onClose={() => setAdding(false)} />}
    </>
  );
}
