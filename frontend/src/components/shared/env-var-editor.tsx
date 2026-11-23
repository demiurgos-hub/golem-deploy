import { Plus, Trash2 } from "lucide-react";

interface EnvVarEditorProps {
  value: Record<string, string>;
  onChange: (value: Record<string, string>) => void;
}

export function EnvVarEditor({ value, onChange }: EnvVarEditorProps) {
  const entries = Object.entries(value);

  const addEntry = () => {
    onChange({ ...value, "": "" });
  };

  const removeEntry = (key: string) => {
    const next = { ...value };
    delete next[key];
    onChange(next);
  };

  const updateKey = (oldKey: string, newKey: string) => {
    const next: Record<string, string> = {};
    for (const [k, v] of Object.entries(value)) {
      next[k === oldKey ? newKey : k] = v;
    }
    onChange(next);
  };

  const updateValue = (key: string, newValue: string) => {
    onChange({ ...value, [key]: newValue });
  };

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium text-foreground">
          Environment Variables
        </span>
        <button
          type="button"
          onClick={addEntry}
          className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-primary hover:bg-accent"
        >
          <Plus className="h-3 w-3" /> Add
        </button>
      </div>
      {entries.map(([key, val], i) => (
        <div key={i} className="flex gap-2">
          <input
            type="text"
            value={key}
            onChange={(e) => updateKey(key, e.target.value)}
            placeholder="KEY"
            className="flex-1 rounded-md border border-input bg-background px-2 py-1 text-sm text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring"
          />
          <input
            type="text"
            value={val}
            onChange={(e) => updateValue(key, e.target.value)}
            placeholder="value"
            className="flex-1 rounded-md border border-input bg-background px-2 py-1 text-sm text-foreground placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring"
          />
          <button
            type="button"
            onClick={() => removeEntry(key)}
            className="rounded-md p-1 text-muted-foreground hover:text-destructive"
          >
            <Trash2 className="h-4 w-4" />
          </button>
        </div>
      ))}
      {entries.length === 0 && (
        <p className="text-xs text-muted-foreground">
          No environment variables defined
        </p>
      )}
    </div>
  );
}
