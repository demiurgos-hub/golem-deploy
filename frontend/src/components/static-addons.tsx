import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Pencil, Plus, X } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError, sentence } from "@/api/errors";
import { useStaticFiles } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Project, StaticAddon } from "@/api/types.gen";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox, FieldError, Input, Select } from "@/components/ui/input";
import { Section } from "@/components/ui/page";
import { ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { localId } from "@/lib/utils";

/** The daemon's limit on add-ons per project. */
export const MAX_STATIC_ADDONS = 16;

const TARGET_SEGMENT = /^(?!-)[A-Za-z0-9._-]+$/;

/**
 * Mirrors the daemon's target rule for early feedback: a relative path below
 * the game root ("." or empty for the root), segments of letters, digits,
 * '.', '_' and '-' separated by '/', no "..", no ".git", none starting with
 * '-'. The daemon normalises ("./assets/" becomes "assets") and decides.
 */
export function validateAddonTarget(raw: string): string | null {
  const t = raw.trim().replace(/^\.\/+/, "").replace(/\/+$/, "");
  if (t === "" || t === ".") return null;
  if (t.startsWith("/")) return "Use a path relative to the game root, without a leading '/'.";
  for (const seg of t.split(/\/+/)) {
    if (seg === "..") return "The path may not leave the game root ('..').";
    if (seg === ".git") return "The path may not contain .git.";
    if (seg === ".") return "Remove the '.' segments from the path.";
    if (!TARGET_SEGMENT.test(seg)) return "Use letters, digits, '.', '_', '-' and '/' only, with no segment starting with '-'.";
  }
  return null;
}

/** The project's static add-ons and, for admins, their editor. */
export function StaticAddonsSection({ project, isAdmin }: { project: Project; isAdmin: boolean }) {
  const [editing, setEditing] = useState(false);
  return (
    <Section
      title="Static add-ons"
      description={
        <>
          ZIP files from the <Link to="/static" className="underline underline-offset-2">static store</Link> that every build extracts
          below the game root, for assets too large for Git. A new version uploaded to the store is used by the next build.
        </>
      }
      actions={
        isAdmin && (
          <Button size="sm" variant="outline" onClick={() => setEditing(true)}>
            <Pencil />
            Edit add-ons
          </Button>
        )
      }
    >
      {project.static_addons.length === 0 ? (
        <p className="text-sm text-muted-foreground">No static add-ons. Builds use the repository as it is.</p>
      ) : (
        <ul className="flex flex-col gap-1.5 text-sm" aria-label="Static add-ons">
          {project.static_addons.map((a, i) => (
            <li key={`${a.name}-${i}`} className="flex flex-wrap items-center gap-2">
              <code className="font-mono text-xs">{a.name}</code>
              <span className="text-muted-foreground">extracted to</span>
              <code className="font-mono text-xs">{a.target === "." || a.target === "" ? ". (the game root)" : a.target}</code>
            </li>
          ))}
        </ul>
      )}
      {editing && <EditAddonsDialog project={project} onClose={() => setEditing(false)} />}
    </Section>
  );
}

interface AddonRow {
  key: string;
  name: string;
  target: string;
}

type RowErrors = Record<string, { name?: string | null; target?: string | null }>;

function EditAddonsDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const files = useStaticFiles();
  const [rows, setRows] = useState<AddonRow[]>(() => project.static_addons.map((a) => ({ key: localId("addon"), ...a })));
  const [errors, setErrors] = useState<RowErrors>({});
  const [listError, setListError] = useState<string | null>(null);
  const zips = (files.data?.items ?? []).filter((f) => f.is_zip);

  const save = useMutation({
    mutationFn: (addons: StaticAddon[]) => api.projects.update(project.id, { static_addons: addons }),
    onSuccess: (p) => {
      queryClient.setQueryData(qk.projects.detail(p.id), p);
      void queryClient.invalidateQueries({ queryKey: qk.projects.list });
      toast.success(p.static_addons.length ? `Static add-ons of ${p.name} saved. The next build uses them.` : `Static add-ons of ${p.name} removed.`);
      onClose();
    },
    onError: (err) => {
      // static_addons[i].name / static_addons[i].target go to that row.
      const m = isApiError(err) && err.code === "validation_failed" ? /^static_addons\[(\d+)\]\.(name|target)$/.exec(err.field ?? "") : null;
      if (m && isApiError(err)) {
        const row = rows[Number(m[1])];
        if (row) setErrors({ [row.key]: { [m[2]]: sentence(err.message) } });
      } else if (isApiError(err) && err.field === "static_addons") {
        setListError(sentence(err.message));
      }
    },
  });

  const update = (key: string, patch: Partial<AddonRow>) => {
    setRows((r) => r.map((x) => (x.key === key ? { ...x, ...patch } : x)));
    setErrors((e) => ({ ...e, [key]: {} }));
  };

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs: RowErrors = {};
    for (const r of rows) {
      const name = r.name ? null : "Choose a ZIP file.";
      const target = validateAddonTarget(r.target);
      if (name || target) errs[r.key] = { name, target };
    }
    setErrors(errs);
    setListError(rows.length > MAX_STATIC_ADDONS ? `A project can have at most ${MAX_STATIC_ADDONS} static add-ons.` : null);
    if (Object.keys(errs).length || rows.length > MAX_STATIC_ADDONS) return;
    save.mutate(rows.map((r) => ({ name: r.name, target: r.target.trim() || "." })));
  }

  const routed =
    isApiError(save.error) && (/^static_addons\[\d+\]\.(name|target)$/.test(save.error.field ?? "") || save.error.field === "static_addons");

  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={`Static add-ons of ${project.name}`}
      description="Each ZIP is extracted at its target path below the game root, in the build and in the image. Two add-ons may not provide the same file."
      size="lg"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form="edit-addons" loading={save.isPending} disabled={!files.isSuccess}>
            Save add-ons
          </Button>
        </>
      }
    >
      {files.isPending ? (
        <LoadingState label="Loading static files" />
      ) : files.isError ? (
        <ErrorState error={files.error} title="Could not load the static files" onRetry={() => void files.refetch()} />
      ) : (
        <form id="edit-addons" onSubmit={submit} noValidate className="flex flex-col gap-3">
          {zips.length === 0 && (
            <Notice tone="info" title="No ZIP files in the static store">
              Upload a ZIP archive on the <Link to="/static" className="underline underline-offset-2">Static files</Link> page first.
            </Notice>
          )}
          {rows.length === 0 && <p className="text-sm text-muted-foreground">No add-ons. Saving removes every add-on from the project.</p>}
          <ul className="flex flex-col gap-3">
            {rows.map((r, i) => {
              const missing = r.name !== "" && !zips.some((z) => z.name === r.name);
              return (
                <li key={r.key} className="flex flex-col gap-1.5 rounded-md border border-border p-3" data-addon-row={i}>
                  <div className="grid gap-2 sm:grid-cols-[1fr_1fr_auto] sm:items-start">
                    <div className="flex flex-col gap-1">
                      <label htmlFor={`${r.key}-name`} className="text-xs font-medium">
                        ZIP file
                      </label>
                      <Select
                        id={`${r.key}-name`}
                        aria-label={`ZIP file of add-on ${i + 1}`}
                        aria-invalid={errors[r.key]?.name ? true : undefined}
                        value={r.name}
                        onChange={(e) => update(r.key, { name: e.target.value })}
                      >
                        <option value="">Choose a ZIP file</option>
                        {missing && <option value={r.name}>{r.name} (not a ZIP in the store)</option>}
                        {zips.map((z) => (
                          <option key={z.name} value={z.name}>
                            {z.name}
                          </option>
                        ))}
                      </Select>
                      <FieldError>{errors[r.key]?.name}</FieldError>
                    </div>
                    <div className="flex flex-col gap-1">
                      <label htmlFor={`${r.key}-target`} className="text-xs font-medium">
                        Target path
                      </label>
                      <Input
                        id={`${r.key}-target`}
                        aria-label={`Target path of add-on ${i + 1}`}
                        aria-invalid={errors[r.key]?.target ? true : undefined}
                        spellCheck={false}
                        placeholder=". (the game root)"
                        value={r.target}
                        onChange={(e) => update(r.key, { target: e.target.value })}
                      />
                      <FieldError>{errors[r.key]?.target}</FieldError>
                    </div>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="sm:mt-5"
                      aria-label={`Remove add-on ${i + 1}`}
                      onClick={() => setRows((rs) => rs.filter((x) => x.key !== r.key))}
                    >
                      <X />
                    </Button>
                  </div>
                </li>
              );
            })}
          </ul>
          <div>
            <Button
              size="sm"
              variant="outline"
              disabled={rows.length >= MAX_STATIC_ADDONS}
              onClick={() => setRows((rs) => [...rs, { key: localId("addon"), name: zips[0]?.name ?? "", target: "" }])}
            >
              <Plus />
              Add add-on
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            Target paths are relative to the game root, for example <code className="font-mono">assets/maps</code>; leave it empty for
            the root itself.
          </p>
          {listError && <FieldError>{listError}</FieldError>}
          {save.isError && !routed && <ErrorText error={save.error} />}
        </form>
      )}
    </Dialog>
  );
}

/** Whether builds may use a Dockerfile from the repository (admin setting, off by default). */
export function BuildSettingsSection({ project, isAdmin }: { project: Project; isAdmin: boolean }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const update = useMutation({
    mutationFn: (allow: boolean) => api.projects.update(project.id, { allow_custom_dockerfile: allow }),
    onSuccess: (p) => {
      queryClient.setQueryData(qk.projects.detail(p.id), p);
      void queryClient.invalidateQueries({ queryKey: qk.projects.list });
      toast.success(p.allow_custom_dockerfile ? `${p.name} may now build with its own Dockerfile.` : `${p.name} builds with the generated Dockerfile only.`);
    },
    onError: (err) => toast.error(err, "Could not change the Dockerfile setting"),
  });
  const checked = update.isPending ? (update.variables ?? project.allow_custom_dockerfile) : project.allow_custom_dockerfile;
  return (
    <Section title="Build settings">
      <div className="flex flex-col gap-2 text-sm">
        {isAdmin ? (
          <Checkbox
            label="Allow the repository's own Dockerfile"
            checked={checked}
            disabled={update.isPending}
            onChange={(e) => update.mutate(e.target.checked)}
          />
        ) : (
          <p>
            Custom Dockerfile: <span className="font-medium">{project.allow_custom_dockerfile ? "allowed" : "not allowed"}</span>
            <span className="text-muted-foreground"> (an administrator can change this)</span>
          </p>
        )}
        <p className="text-xs text-muted-foreground">
          Off by default: builds use the Dockerfile Golem Deploy generates, and a repository whose{" "}
          <code className="font-mono">golem-deploy.yaml</code> asks for its own <code className="font-mono">dockerfile</code> fails to
          build. Turning it on lets the repository decide every build step. Docker&apos;s layer cache is shared by every project on
          this host, so enabling it means trusting this repository, and everyone who can push to it, with that shared build cache.
        </p>
      </div>
    </Section>
  );
}
