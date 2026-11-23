import { useCallback, useState } from "react";

interface PersistedDefaults {
  get: (key: string) => string;
  set: (key: string, value: string) => void;
}

interface RunFormConfig {
  container_name: string;
  port: string;
  label: string;
  env_vars?: Record<string, string>;
}

export interface RunPayloadFields {
  container_name: string;
  port: string;
  label: string;
  env_vars?: Record<string, string>;
}

export interface MassRunPayloadFields extends Omit<RunPayloadFields, "env_vars"> {
  env_vars: Record<string, string>;
}

export interface RunFormState {
  containerName: string;
  setContainerName: (value: string) => void;
  port: string;
  setPort: (value: string) => void;
  label: string;
  setLabel: (value: string) => void;
  envVars: Record<string, string>;
  setEnvVars: (value: Record<string, string>) => void;
  loadDefaults: (defaults: PersistedDefaults) => void;
  loadConfig: (config: RunFormConfig) => void;
  persistDefaults: (defaults: PersistedDefaults) => void;
  asRunPayloadFields: () => RunPayloadFields;
  asMassRunPayloadFields: () => MassRunPayloadFields;
}

export function useRunFormState(): RunFormState {
  const [containerName, setContainerName] = useState("");
  const [port, setPort] = useState("");
  const [label, setLabel] = useState("");
  const [envVars, setEnvVars] = useState<Record<string, string>>({});

  const loadDefaults = useCallback((defaults: PersistedDefaults) => {
    setContainerName("");
    setPort(defaults.get("port"));
    setLabel(defaults.get("label"));
    setEnvVars({});
  }, []);

  const loadConfig = useCallback((config: RunFormConfig) => {
    setContainerName(config.container_name);
    setPort(config.port);
    setLabel(config.label);
    setEnvVars(config.env_vars || {});
  }, []);

  const persistDefaults = useCallback(
    (defaults: PersistedDefaults) => {
      defaults.set("port", port);
      defaults.set("label", label);
    },
    [label, port],
  );

  const asRunPayloadFields = useCallback(
    () => ({
      container_name: containerName,
      port,
      label,
      env_vars: Object.keys(envVars).length > 0 ? envVars : undefined,
    }),
    [containerName, envVars, label, port],
  );

  const asMassRunPayloadFields = useCallback(
    () => ({
      container_name: containerName,
      port,
      label,
      env_vars: envVars,
    }),
    [containerName, envVars, label, port],
  );

  return {
    containerName,
    setContainerName,
    port,
    setPort,
    label,
    setLabel,
    envVars,
    setEnvVars,
    loadDefaults,
    loadConfig,
    persistDefaults,
    asRunPayloadFields,
    asMassRunPayloadFields,
  };
}
