import { EnvVarEditor } from "@/components/shared/env-var-editor";
import type { RunFormState } from "@/hooks/use-run-form-state";

interface RunFieldsProps {
  form: RunFormState;
  placeholders?: {
    containerName?: string;
    port?: string;
    label?: string;
  };
}

interface TextFieldProps {
  label: string;
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
}

const inputClassName =
  "w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring";

function TextField({
  label,
  value,
  onChange,
  placeholder,
}: TextFieldProps) {
  return (
    <div className="space-y-2">
      <label className="text-sm font-medium">{label}</label>
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className={inputClassName}
        placeholder={placeholder}
      />
    </div>
  );
}

export function RunFields({
  form,
  placeholders,
}: RunFieldsProps) {
  return (
    <>
      <TextField
        label="Container Name"
        value={form.containerName}
        onChange={form.setContainerName}
        placeholder={placeholders?.containerName}
      />
      <TextField
        label="Port"
        value={form.port}
        onChange={form.setPort}
        placeholder={placeholders?.port}
      />
      <TextField
        label="Label"
        value={form.label}
        onChange={form.setLabel}
        placeholder={placeholders?.label}
      />
      <EnvVarEditor value={form.envVars} onChange={form.setEnvVars} />
    </>
  );
}
