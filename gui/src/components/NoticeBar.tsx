import { useStore } from "@/state/store";

export function NoticeBar() {
  const { state, effects } = useStore();
  if (!state.notice) return null;
  return (
    <div className={`notice notice-${state.notice.kind}`}>
      <span>{state.notice.text}</span>
      <button className="btn-ghost" onClick={() => effects.setNotice(null)}>
        dismiss
      </button>
    </div>
  );
}
