// App.tsx — the top-level layout (plan §6 UI regions, CodexMonitor layout):
//   left sidebar (projects -> tasks) | center (task timeline + composer) | right
//   panel (workflow / task metadata). Secondary views (Workflows, Agents,
//   Settings) replace the center+right region when selected.

import { Sidebar } from "@/components/Sidebar";
import { TaskTimeline } from "@/components/TaskTimeline";
import { Composer } from "@/components/Composer";
import { RightPanel } from "@/components/RightPanel";
import { WorkflowsView } from "@/components/WorkflowsView";
import { AgentsView } from "@/components/AgentsView";
import { SettingsView } from "@/components/SettingsView";
import { TopBar } from "@/components/TopBar";
import { NoticeBar } from "@/components/NoticeBar";
import { useStore } from "@/state/store";
import { activeTask } from "@/state/selectors";

export default function App() {
  const { state } = useStore();
  const task = activeTask(state);

  return (
    <div className="app-root">
      <TopBar />
      <NoticeBar />
      <div className="app-body">
        <Sidebar />
        <main className="app-main">
          {state.view === "tasks" && (
            <div className="task-pane">
              <section className="task-center">
                <TaskTimeline />
                <Composer />
              </section>
              <RightPanel task={task} />
            </div>
          )}
          {state.view === "workflows" && <WorkflowsView />}
          {state.view === "agents" && <AgentsView />}
          {state.view === "settings" && <SettingsView />}
        </main>
      </div>
    </div>
  );
}
