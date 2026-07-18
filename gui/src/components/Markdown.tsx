// Markdown — renders assistant_* transcript text as markdown, mirroring lazy's
// Detail pane (plan §6 "assistant_* via markdown").

import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { handleMarkdownLinkClick } from "./markdownLinks";

export function Markdown({ text }: { text: string }) {
  return (
    <div className="markdown">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ node: _node, ...props }) => (
            <a
              {...props}
              // onAuxClick covers middle-click, which would otherwise navigate
              // the WebView by href in bypass of onClick scheme validation. The
              // handler inspects event.button so right-click / back-forward
              // buttons (which also dispatch auxclick) fall through untouched.
              onClick={(event) => handleMarkdownLinkClick(event, props.href)}
              onAuxClick={(event) => handleMarkdownLinkClick(event, props.href)}
            />
          ),
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}
