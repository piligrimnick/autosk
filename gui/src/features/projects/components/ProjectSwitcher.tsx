// ProjectSwitcher — the center-header dropdown (redesign plan §8.2, decision
// #7). Shows the active project name; the menu switches project, removes one
// from the registry, and opens the add/init modal.

import { useState } from "react";
import { useStore } from "@/state/store";
import * as ipc from "@/services/ipc";
import { Menu, MenuDivider, MenuItem, MenuLabel } from "@/features/shared/Menu";
import { AddProjectModal } from "./AddProjectModal";

export function ProjectSwitcher() {
  const { state, effects } = useStore();
  const [anchor, setAnchor] = useState<DOMRect | null>(null);
  const [adding, setAdding] = useState(false);
  const activeName = state.projects.find((p) => p.root === state.activeProject)?.name ?? "No project";
  const close = () => setAnchor(null);

  const remove = async (root: string, name: string) => {
    close();
    if (!confirm(`Remove ${name} from the registry? (the project's .autosk/db is left untouched)`)) return;
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
        <MenuDivider />
        <MenuItem onClick={() => { close(); setAdding(true); }}>Add / init project…</MenuItem>
      </Menu>

      {adding && <AddProjectModal onClose={() => setAdding(false)} />}
    </>
  );
}
