import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import { StoreProvider } from "@/state/store";
import "./styles/base.css";
import "./styles/layout.css";
import "./styles/sidebar.css";
import "./styles/timeline.css";
import "./styles/composer.css";
import "./styles/panel.css";
import "./styles/views.css";
import "./styles/modal.css";

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <StoreProvider>
      <App />
    </StoreProvider>
  </React.StrictMode>,
);
