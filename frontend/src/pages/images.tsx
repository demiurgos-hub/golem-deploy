import { useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { useAccount, useImages, useProjects, useServers } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Image, Server } from "@/api/types.gen";
import { CopyButton } from "@/components/ui/copy-button";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable } from "@/components/ui/data-table";
import { ConfirmDialog } from "@/components/ui/dialog";
import { PageHeader } from "@/components/ui/page";
import { EmptyState, ErrorState, LoadingState } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatBytes, formatDateTime, formatRelative, shortCommit, shortImageId } from "@/lib/format";

/** The servers configured with the image (Image.server_ids), named when the server list is loaded. */
function usersOf(image: Image, servers: readonly Server[]): { id: number; name: string }[] {
  return image.server_ids.map((id) => ({ id, name: servers.find((s) => s.id === id)?.name ?? `server ${id}` }));
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
      <PageHeader
        title="Images"
        description="Docker images of this installation, made by project builds. Images of builds are removed by deleting the build; only images no build records (for example one a build could not remove) are deleted here."
      />
      {images.isPending ? (
        <LoadingState label="Loading images" />
      ) : images.isError ? (
        <ErrorState error={images.error} title="Could not load images" onRetry={() => void images.refetch()} />
      ) : images.data.items.length === 0 ? (
        <EmptyState
          title="No images yet"
          description="Images are made by builds: open a project and start a build. Every successful build records its image here."
        />
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
            {
              key: "commit",
              header: "Commit",
              hideOnSmall: true,
              cell: (i) =>
                i.commit ? (
                  <span className="inline-flex items-center gap-1">
                    <code className="font-mono text-xs" title={i.commit}>
                      {shortCommit(i.commit)}
                    </code>
                    <CopyButton value={i.commit} size="icon" variant="ghost" label="Copy commit hash" />
                  </span>
                ) : (
                  <span className="text-muted-foreground">-</span>
                ),
            },
            {
              key: "builds",
              header: "Builds",
              cell: (i) =>
                i.build_ids.length ? (
                  <div className="flex flex-wrap gap-x-2">
                    {i.build_ids.map((id) => (
                      <Link key={id} to="/builds/$id" params={{ id: String(id) }} className="hover:underline">
                        #{id}
                      </Link>
                    ))}
                  </div>
                ) : (
                  <span className="text-muted-foreground" title="No build records this image: it can be deleted here once nothing uses it.">
                    None
                  </span>
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
                return i.in_use ? (
                  <Badge tone="info" title="A container was created from this image.">
                    In use by a container
                  </Badge>
                ) : (
                  <span className="text-muted-foreground">Unused</span>
                );
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
              {formatBytes(deleting.size_bytes)})? An image that a build records is deleted with that build instead, and an image
              that a server&apos;s build uses or any container was created from cannot be deleted.
            </>
          )
        }
        confirmLabel="Delete image"
        destructive
        onConfirm={async () => {
          if (!deleting) return;
          // A 409 says why (the build that records it, or that it is in use); the dialog shows it as sent.
          await api.images.remove(deleting.id);
          toast.success("Image deleted.");
          await queryClient.invalidateQueries({ queryKey: qk.images });
        }}
      />
    </>
  );
}
