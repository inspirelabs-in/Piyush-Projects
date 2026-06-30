"use client";

import { createContext, useContext } from "react";

// Stable canvas actions handed to custom nodes via context (not via node data),
// so node data stays pure and React Flow's node memoization holds.
export interface CanvasActions {
  // Enter "focus flow" for a node: isolate its cross-repo connections.
  focusFlow: (id: string) => void;
  // Toggle a function node's inline code panel (the canvas owns this so it can
  // reflow the column and keep expanded cards from overlapping their neighbours).
  toggleFn: (id: string) => void;
}

export const CanvasActionsContext = createContext<CanvasActions>({
  focusFlow: () => {},
  toggleFn: () => {},
});

export function useCanvasActions(): CanvasActions {
  return useContext(CanvasActionsContext);
}
