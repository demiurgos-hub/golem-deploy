import { useCallback } from "react";

const PREFIX = "golem_deploy_default_";

export function usePersistedDefaults() {
  const get = useCallback((key: string): string => {
    return localStorage.getItem(PREFIX + key) ?? "";
  }, []);

  const set = useCallback((key: string, value: string) => {
    localStorage.setItem(PREFIX + key, value);
  }, []);

  return { get, set };
}
