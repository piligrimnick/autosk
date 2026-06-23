// Markdown — renders assistant_* transcript text as markdown, mirroring lazy's
// Detail pane (plan §6 "assistant_* via markdown").

import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

export function Markdown({ text }: { text: string }) {
  return (
    <div className="markdown">
      <ReactMarkdown remarkPlugins={[remarkGfm]}>{text}</ReactMarkdown>
    </div>
  );
}
