import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { ApiError, isApiError } from "@/api/errors";
import { useAccount, useVolumes } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Volume } from "@/api/types.gen";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable } from "@/components/ui/data-table";
import { ConfirmDialog } from "@/components/ui/dialog";
import { PageHeader } from "@/components/ui/page";
import { EmptyState, ErrorState, LoadingState } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, formatRelative } from "@/lib/format";

export function VolumesPage() {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const volumes = useVolumes();
  const account = useAccount();
  const isAdmin = account.data?.role === "admin";
  const [deleting, setDeleting] = useState<Volume | null>(null);

  return (
    <>
      <PageHeader
        title="Volumes"
        description="Docker volumes that hold servers' saved data. Each server keeps its world on its current data volume, which goes away with the server and is replaced by a restore or reset. Other volumes are leftovers an administrator can delete here."
      />
      {volumes.isPending ? (
        <LoadingState label="Loading volumes" />
      ) : volumes.isError ? (
        <ErrorState error={volumes.error} title="Could not load volumes" onRetry={() => void volumes.refetch()} />
      ) : volumes.data.items.length === 0 ? (
        <EmptyState
          title="No volumes"
          description="A server gets its data volume when it first starts. Volumes that Golem Deploy manages are listed here."
        />
      ) : (
        <DataTable
          label="Volumes"
          data={volumes.data.items}
          rowKey={(v) => v.name}
          initialSort={{ key: "name", asc: true }}
          columns={[
            { key: "name", header: "Name", sortValue: (v) => v.name, cell: (v) => <code className="font-mono text-xs">{v.name}</code> },
            {
              key: "server",
              header: "Server",
              sortValue: (v) => v.server_name,
              cell: (v) =>
                v.server_id === null ? (
                  <span className="inline-flex items-center gap-1.5">
                    <span className="text-muted-foreground">{v.server_name || "-"}</span>
                    <Badge tone="warning">Orphaned</Badge>
                  </span>
                ) : (
                  <Link to="/servers/$id" params={{ id: String(v.server_id) }} className="hover:underline">
                    {v.server_name}
                  </Link>
                ),
            },
            {
              key: "current",
              header: "Role",
              sortValue: (v) => (v.current ? 0 : 1),
              cell: (v) =>
                v.current ? (
                  <Badge tone="success" title="The server's saved world lives here. It is deleted with the server.">
                    Current data
                  </Badge>
                ) : (
                  <span className="text-muted-foreground" title="Not the current data volume of any server: left over after a restore, a reset or a deleted server.">
                    Leftover
                  </span>
                ),
            },
            {
              key: "in_use",
              header: "Attached",
              cell: (v) => (v.in_use ? <Badge tone="info">Mounted</Badge> : <span className="text-muted-foreground">Not mounted</span>),
            },
            {
              key: "created",
              header: "Created",
              hideOnSmall: true,
              sortValue: (v) => v.created_at,
              cell: (v) => <span title={formatDateTime(v.created_at)}>{formatRelative(v.created_at)}</span>,
            },
          ]}
          actions={
            isAdmin
              ? (v) =>
                  v.current ? (
                    <span className="text-xs text-muted-foreground">Deleted with its server</span>
                  ) : (
                    <Button size="sm" variant="outline" aria-label={`Delete volume ${v.name}`} onClick={() => setDeleting(v)}>
                      <Trash2 />
                      Delete
                    </Button>
                  )
              : undefined
          }
        />
      )}
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(o) => !o && setDeleting(null)}
        title="Delete volume"
        description={
          deleting && (
            <>
              Delete <span className="font-medium text-foreground">{deleting.name}</span>
              {deleting.server_name ? ` (earlier data of ${deleting.server_name})` : ""}? It is not any server&apos;s current data, but
              the files in it are lost for good. Backups are not affected. This cannot be undone.
            </>
          )
        }
        confirmLabel="Delete volume"
        destructive
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await api.volumes.remove(deleting.name);
          } catch (err) {
            if (isApiError(err) && err.code === "conflict") {
              throw new ApiError({
                status: err.status,
                code: err.code,
                requestId: err.requestId,
                message: `The volume is mounted by a container${deleting.server_name ? ` (${deleting.server_name})` : ""}. Stop or delete that server first.`,
              });
            }
            throw err;
          }
          toast.success(`Volume ${deleting.name} deleted.`);
          await queryClient.invalidateQueries({ queryKey: qk.volumes });
        }}
      />
    </>
  );
}
