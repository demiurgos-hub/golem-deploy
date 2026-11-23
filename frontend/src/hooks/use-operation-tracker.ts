import {
  createContext,
  useContext,
  useState,
  useEffect,
  type ReactNode,
} from "react";
import { createElement } from "react";
import { apiClient } from "@/api/client";

interface OperationTrackerContextValue {
  operations: Map<string, string>;
}

const OperationTrackerContext =
  createContext<OperationTrackerContextValue | null>(null);

export function OperationTrackerProvider({
  children,
}: {
  children: ReactNode;
}) {
  const [operations, setOperations] = useState<Map<string, string>>(
    new Map(),
  );

  useEffect(() => {
    const unsub = apiClient.onOperationsChange((ops) => setOperations(ops));
    return () => { unsub(); };
  }, []);

  return createElement(
    OperationTrackerContext.Provider,
    { value: { operations } },
    children,
  );
}

export function useOperationTracker() {
  const ctx = useContext(OperationTrackerContext);
  if (!ctx)
    throw new Error(
      "useOperationTracker must be used within OperationTrackerProvider",
    );
  return ctx;
}
