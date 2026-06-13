// useSecondTick — force a re-render once per second while `active` is true.
//
// The Sessions panel's left column shows a running session's live work time
// (now − started_at). React only re-renders on state changes, so without a
// clock that value freezes between session events. This mirrors lazy-mode's TUI
// render loop: a 1s tick that re-renders while at least one session is live, so
// the seconds count up. The interval is torn down the moment nothing is live,
// so an idle panel does no per-second work.

import { useEffect, useState } from "react";

export function useSecondTick(active: boolean): void {
  const [, setTick] = useState(0);
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => setTick((t) => (t + 1) % 60), 1000);
    return () => clearInterval(id);
  }, [active]);
}
