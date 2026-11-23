import { useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { ApiError, isApiError } from "@/api/errors";
import { useAccount, useImages, useProjects, useServers } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Image, Server } from "@/api/types.gen";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable } from "@/components/ui/data-table";
import { ConfirmDialog } from "@/components/ui/dialog";
import { PageHeader } from "@/components/ui/page";
import { EmptyState, ErrorState, LoadingState } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatBytes, formatDateTime, formatRelative, shortImageId } from "@/lib/format";

/** Servers whose image reference is one of the image's tags or its id. */
function usersOf(image: Image, servers: readonly Server[]): Server[] {
  const refs = new Set([...image.tags, image.id, shortImageId(image.id)]);
  return servers.filter((s) => refs.has(s.image));
}

export function ImagesPage() {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const images = useImages();
  const servers = useServers();
  const projects = useProjects();
  const account = useAccount();
  const isAdmin = account.data?.role === "admin";
  const [deleting, setDeleting] = useState<Image | null>(null);
  const projectNames = useMemo(() => new Map((projects.data?.items ?? []).map((p) => [p.id, p.name])), [projects.data]);
  const serverList = servers.data?.items ?? [];

  return (
    <>
      <PageHeader title="Images" description="Docker images built and managed by Golem Deploy. Other images on the host are not listed." />
      {images.isPending ? (
        <LoadingState label="Loading images" />
      ) : images.isError ? (
        <ErrorState error={images.error} title="Could not load images" onRetry={() => void images.refetch()} />
      ) : images.data.items.length === 0 ? (
        <EmptyState title="No managed images" description="Images appear here once a project has been built." />
      ) : (
        <DataTable
          label="Images"
          data={images.data.items}
          rowKey={(i) => i.id}
          initialSort={{ key: "created", asc: false }}
          columns={[
            {
              key: "tags",
              header: "Tags",
              cell: (i) =>
                i.tags.length ? (
                  <div className="flex flex-wrap gap-1">
                    {i.tags.map((t) => (
                      <code key={t} className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">
                        {t}
                      </code>
                    ))}
                  </div>
                ) : (
                  <span className="text-muted-foreground">untagged</span>
                ),
            },
            { key: "id", header: "Id", hideOnSmall: true, cell: (i) => <code className="font-mono text-xs" title={i.id}>{shortImageId(i.id)}</code> },
            {
              key: "project",
              header: "Project",
              hideOnSmall: true,
              cell: (i) =>
                i.project_id === null ? (
                  <span className="text-muted-foreground">-</span>
                ) : (
                  <Link to="/projects/$id" params={{ id: String(i.project_id) }} className="hover:underline">
                    {projectNames.get(i.project_id) ?? `project ${i.project_id}`}
                  </Link>
                ),
            },
            { key: "size", header: "Size", sortValue: (i) => i.size_bytes, cell: (i) => formatBytes(i.size_bytes) },
            {
              key: "created",
              header: "Age",
              sortValue: (i) => i.created_at,
              cell: (i) => <span title={formatDateTime(i.created_at)}>{formatRelative(i.created_at)}</span>,
            },
            {
              key: "used",
              header: "Used by",
              cell: (i) => {
                const users = usersOf(i, serverList);
                if (users.length) {
                  return (
                    <div className="flex flex-wrap gap-x-2">
                      {users.map((s) => (
                        <Link key={s.id} to="/servers/$id" params={{ id: String(s.id) }} className="hover:underline">
                          {s.name}
                        </Link>
                      ))}
                    </div>
                  );
                }
                return i.in_use ? <Badge tone="info">In use</Badge> : <span className="text-muted-foreground">Unused</span>;
              },
            },
          ]}
          actions={
            isAdmin
              ? (i) => (
                  <Button size="sm" variant="outline" aria-label={`Delete image ${i.tags[0] ?? shortImageId(i.id)}`} onClick={() => setDeleting(i)}>
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
        title="Delete image"
        description={
          deleting && (
            <>
              Delete <span className="font-medium text-foreground">{deleting.tags.join(", ") || shortImageId(deleting.id)}</span> (
              {formatBytes(deleting.size_bytes)})? Images used by a server cannot be deleted.
            </>
          )
        }
        confirmLabel="Delete image"
        destructive
        onConfirm={async () => {
          if (!deleting) return;
          try {
            await api.images.remove(deleting.id);
          } catch (err) {
            if (isApiError(err) && err.code === "conflict") {
              const users = usersOf(deleting, serverList).map((s) => s.name);
              throw new ApiError({
                status: err.status,
                code: err.code,
                requestId: err.requestId,
                message: users.length
                  ? `The image is still used by ${users.join(", ")}. Delete those servers or switch them to another image first.`
                  : "The image is still used by a server container. Delete that server or switch it to another image first.",
              });
            }
            throw err;
          }
          toast.success("Image deleted.");
          await queryClient.invalidateQueries({ queryKey: qk.images });
        }}
      />
    </>
  );
}
