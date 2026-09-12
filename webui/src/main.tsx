import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

// Self-hosted brand faces (@fontsource): DM Sans for figures and Latin runs,
// Cormorant Garamond for the wordmark, Noto Sans Arabic for the interface.
// Bundled, never fetched from a CDN — the CSP forbids it and §12 demands the
// screen look identical on a desk with no outside internet.
import "@fontsource/dm-sans/400.css";
import "@fontsource/dm-sans/500.css";
import "@fontsource/dm-sans/700.css";
import "@fontsource/cormorant-garamond/600.css";
import "@fontsource/cormorant-garamond/700.css";
import "@fontsource/noto-sans-arabic/400.css";
import "@fontsource/noto-sans-arabic/500.css";
import "@fontsource/noto-sans-arabic/700.css";

import "./design/tokens.css";
import "./design/base.css";
import "./design/components.css";
import "./design/shell.css";
import "./design/print.css";

import { App } from "./app/App";
import { ErrorBoundary } from "./app/ErrorBoundary";
import { SessionProvider } from "./app/session";
import { isRefusal } from "./api/errors";

/**
 * Query defaults.
 *
 * No optimistic updates anywhere — §03 principle 07 forbids them on money, and
 * a default that permits them for "harmless" screens is a default someone
 * eventually applies to a payment. Mutations here always wait for the server.
 *
 * Retries are off for anything the server refused: a refusal is an answer, and
 * repeating a refused command three times only delays telling the operator.
 * A dropped connection is retried once, because that one may genuinely be
 * transient — and the commands that move money carry an idempotency key that
 * makes the retry safe.
 */
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      refetchOnWindowFocus: false,
      retry: (failureCount, error) => {
        if (isRefusal(error) && error.kind !== "unreachable") return false;
        return failureCount < 1;
      },
    },
    mutations: {
      retry: false,
    },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter basename="/app">
      <QueryClientProvider client={queryClient}>
        <ErrorBoundary>
          <SessionProvider>
            <App />
          </SessionProvider>
        </ErrorBoundary>
      </QueryClientProvider>
    </BrowserRouter>
  </StrictMode>,
);
