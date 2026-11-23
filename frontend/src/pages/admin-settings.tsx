import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useApi } from "@/app/services";
import { useSettings } from "@/api/queries";
import { qk } from "@/api/query-keys";
import { Checkbox } from "@/components/ui/input";
import { PageHeader, Section } from "@/components/ui/page";
import { ErrorState, LoadingState } from "@/components/ui/states";
import { Spinner } from "@/components/ui/spinner";
import { useToast } from "@/components/ui/toast";

export function AdminSettingsPage() {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const settings = useSettings();

  const update = useMutation({
    mutationFn: (registration_enabled: boolean) => api.system.updateSettings({ registration_enabled }),
    onSuccess: (s) => {
      queryClient.setQueryData(qk.settings, s);
      void queryClient.invalidateQueries({ queryKey: qk.status });
      toast.success(s.registration_enabled ? "Self-registration turned on." : "Self-registration turned off.");
    },
    onError: (err) => toast.error(err, "Could not save the setting"),
  });

  return (
    <>
      <PageHeader title="Settings" description="Host-wide settings stored by the daemon." />
      <Section
        title="Self-registration"
        description="When on, anyone who can reach this page can request a developer account at /register. New accounts cannot sign in until an administrator approves them."
      >
        {settings.isPending ? (
          <LoadingState />
        ) : settings.isError ? (
          <ErrorState error={settings.error} onRetry={() => void settings.refetch()} />
        ) : (
          <div className="flex items-center gap-3">
            <Checkbox
              label="Allow self-registration"
              checked={update.isPending ? (update.variables ?? settings.data.registration_enabled) : settings.data.registration_enabled}
              disabled={update.isPending}
              onChange={(e) => update.mutate(e.target.checked)}
            />
            {update.isPending && <Spinner label="Saving" />}
          </div>
        )}
      </Section>
    </>
  );
}
