import React from "react";
import ReactDOM from "react-dom/client";
import { App } from "./ui/App";
import { ErrorBoundary } from "./ui/ErrorBoundary";
import { RuntimeClient } from "./runtime/client";
import "./ui/styles.css";
import "./ui/GitTools.css";

const client = new RuntimeClient();

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ErrorBoundary>
      <App client={client} />
    </ErrorBoundary>
  </React.StrictMode>
);
