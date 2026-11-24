import { useEffect, useRef, useState } from "react";
import { useApi } from "@/app/services";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Section } from "@/components/ui/page";
import { ErrorText } from "@/components/ui/states";
import { connectionSteps, runConnectionTest, type Step, type StepResult } from "@/lib/connection-test";

const stepKeys = Object.keys(connectionSteps) as Step[];
const tones = { running: "info", passed: "success", failed: "danger", skipped: "neutral" } as const;
const labels = { running: "Running", passed: "Passed", failed: "Failed", skipped: "Skipped" } as const;

export function ConnectionTest({ serverId }: { serverId: number }) {
  const api = useApi();
  const current = useRef<AbortController | null>(null);
  const [results, setResults] = useState<Partial<Record<Step, StepResult>> | null>(null);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    function cancel() {
      current.current?.abort();
    }
    window.addEventListener("pagehide", cancel);
    return () => {
      cancel();
      current.current = null;
      window.removeEventListener("pagehide", cancel);
    };
  }, []);

  async function run() {
    if (current.current) return;
    const controller = new AbortController();
    current.current = controller;
    setResults({});
    setError(null);
    setRunning(true);
    try {
      await runConnectionTest(api.servers, serverId, controller.signal, (step, result) => {
        setResults((previous) => ({ ...previous, [step]: result }));
      });
    } catch (err) {
      if (!controller.signal.aborted) setError(err);
    } finally {
      if (current.current === controller) {
        current.current = null;
        if (controller.signal.aborted) setResults(null);
        setRunning(false);
      }
    }
  }

  return (
    <Section title="Connection test" description="Test the player connection from this browser. The test opens short connections the game may count as a player joining and leaving.">
      <div><Button size="sm" variant="outline" loading={running} onClick={() => void run()}>Run connection test</Button></div>
      {error != null && <ErrorText error={error} />}
      {results && (
        <ol aria-live="polite" className="mt-3 divide-y divide-border rounded-md border border-border">
          {stepKeys.map((step) => {
            const result = results[step];
            return (
              <li key={step} className="space-y-1 px-3 py-2 text-sm">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">{connectionSteps[step]}</span>
                  {result ? <Badge tone={tones[result.status]}>{labels[result.status]}</Badge> : <span className="text-muted-foreground">Waiting</span>}
                  {result && result.status !== "running" && (
                    <span className="text-xs tabular-nums text-muted-foreground">{Math.round(result.milliseconds)} ms</span>
                  )}
                </div>
                {result && result.status !== "running" && (
                  <p className={result.status === "failed" ? "text-destructive" : "text-muted-foreground"}>{result.detail}</p>
                )}
              </li>
            );
          })}
        </ol>
      )}
    </Section>
  );
}
