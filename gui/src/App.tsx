// App — mounts the frameless 3-panel shell (redesign plan §4, §5). All UI now
// lives under src/features/*; src/components keeps only the shared primitives
// (Modal, Markdown, NoticeBar, common).

import { AppShell } from "@/features/layout/components/AppShell";

export default function App() {
  return <AppShell />;
}
