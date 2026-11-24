import { afterEach, describe, expect, it, vi } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import type { Backup, Server, Volume } from "@/api/types.gen";
import { OMITTED_HINT, backupKindLabel, schemaMatch } from "@/components/backups";
import { jobKindLabel } from "@/components/jobs";
import { formatDurationWords } from "@/lib/format";
import { checkServerImport } from "@/lib/server-import";
import { trimBackupNote, validateBackupInterval, validateBackupKeep, validateBackupNote, validateSnapshotInterval } from "@/lib/validators";
import { serverFilterOptions } from "@/pages/backups";
import { account, errorResponse, job, jsonResponse, streamResponse, type Handler } from "@/test/fake-api";
import { COMMIT, FINGERPRINT, backupFixture, backupList, buildFixture, projectFixture, runtimeFixture, serverFixture } from "@/test/fixtures";
import { renderApp } from "@/test/render-app";

// Saved data and backups (docs/API.md, "Saved data and backups").

const developer = account({ id: 2, role: "developer", username: "dev" });
const OLD_FP = "a0c3d9e1f2b47c55";
const OTHER_COMMIT = "0d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e";

function pageRoutes(s: Server, backups: Backup[] = [backupFixture()], extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    "GET /servers/{id}": () => jsonResponse(200, s),
    "GET /projects": () => jsonResponse(200, { items: [projectFixture()] }),
    "GET /jobs": () => jsonResponse(200, { items: [] }),
    "GET /jobs/{id}/log": () => streamResponse().response,
    "GET /servers/{id}/stats": () => errorResponse(409, "conflict", "the server is not running"),
    "GET /servers/{id}/runtime": () => jsonResponse(200, runtimeFixture()),
    "GET /servers/{id}/logs": () => streamResponse().response,
    "GET /builds": () => jsonResponse(200, { items: [buildFixture({ schema_fingerprint: FINGERPRINT })] }),
    "GET /backups": () => jsonResponse(200, backupList(backups)),
    ...extra,
  };
}

async function dataSection() {
  return screen.findByRole("region", { name: "Saved data and backups" });
}

describe("server page: saved data", () => {
  it("shows where the world lives, its schema, the last backup and the server's backups in plain words", async () => {
    const { calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: pageRoutes(serverFixture(), [
        backupFixture({ id: 42, kind: "scheduled", created_by: null, created_by_username: null, note: "", created_at: "2026-11-22T09:00:00Z" }),
        backupFixture({ id: 41, kind: "pre_restore", schema_fingerprint: OLD_FP, note: "" }),
        backupFixture({ id: 40, size_bytes: 3_500_000 }),
      ]),
    });
    const section = await dataSection();
    expect(within(section).getByText("golem-deploy-data-3-0001")).toBeInTheDocument();
    // Shortened, with the full value in the title.
    expect(within(section).getAllByText(FINGERPRINT.slice(0, 12))[0]).toHaveAttribute("title", FINGERPRINT);
    const table = await within(section).findByRole("table", { name: "Backups of this server" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(3);
    expect(within(rows[0]).getByText("Scheduled")).toBeInTheDocument();
    expect(within(rows[0]).getByText("by the schedule")).toBeInTheDocument();
    expect(within(rows[1]).getByText("Before a restore")).toBeInTheDocument();
    expect(within(rows[1]).getByText("Different schema")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Manual")).toBeInTheDocument();
    expect(within(rows[2]).getByText("3.3 MiB")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Same schema as the build")).toBeInTheDocument();
    expect(within(rows[2]).getByText("before the map update")).toBeInTheDocument();
    expect(within(rows[2]).getByRole("link", { name: `#12 (${COMMIT.slice(0, 12)})` })).toHaveAttribute("href", "/builds/12");
    expect(calls.find((c) => c.path === "/backups")?.query.get("server_id")).toBe("3");
  });

  it("hides the admin actions from developers but lets them back up and download", async () => {
    renderApp({ path: "/servers/3", signedInAs: developer, routes: pageRoutes(serverFixture()) });
    const section = await dataSection();
    await within(section).findByRole("table", { name: "Backups of this server" });
    expect(within(section).getByRole("button", { name: "Download backup 40" })).toBeInTheDocument();
    expect(within(section).getByRole("button", { name: "Back up now" })).toBeEnabled();
    expect(within(section).queryByRole("button", { name: "Restore backup 40" })).toBeNull();
    expect(within(section).queryByRole("button", { name: "Delete backup 40" })).toBeNull();
    expect(within(section).queryByRole("button", { name: "Reset data" })).toBeNull();
    expect(within(section).queryByRole("button", { name: "Restore from any backup" })).toBeNull();
    expect(within(section).getByText("Restoring a backup, resetting the data and deleting backups need an administrator.")).toBeInTheDocument();
  });

  it("backs up now with a note and follows the job inline", async () => {
    const backupJob = job({ id: 90, kind: "server.backup", subject_type: "server", subject_id: "3", status: "running", started_at: "2026-11-24T10:00:00Z" });
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: pageRoutes(serverFixture(), [], {
        "POST /servers/{id}/backups": () => jsonResponse(202, backupJob),
        "GET /jobs/{id}": () => jsonResponse(200, backupJob),
      }),
    });
    const section = await dataSection();
    await user.type(within(section).getByLabelText("Note (optional)"), "  before the patch ");
    await user.click(within(section).getByRole("button", { name: "Back up now" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/servers/3/backups")?.body).toEqual({ note: "before the patch" }));
    const inline = await screen.findByLabelText("Job 90");
    expect(within(inline).getByText("Back up data")).toBeInTheDocument();
    expect(within(section).getByLabelText("Note (optional)")).toHaveValue("");
  });

  it("sends no note when none is given and shows job_in_progress as the daemon worded it", async () => {
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: pageRoutes(serverFixture(), [], {
        "POST /servers/{id}/backups": () => errorResponse(409, "job_in_progress", "another job is already running for this server"),
      }),
    });
    const section = await dataSection();
    await user.click(within(section).getByRole("button", { name: "Back up now" }));
    expect(await within(section).findByText("Another job is already running for this server.")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST" && c.path === "/servers/3/backups")?.body).toEqual({});
  });

  it("offers no backup while the server has no saved data", async () => {
    renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: pageRoutes(
        serverFixture({ state: "missing", container_id: "", desired_state: "stopped", data: { volume: "", schema_fingerprint: "", schema_mismatch: false, last_backup_at: null } }),
        [],
      ),
    });
    const section = await dataSection();
    expect(within(section).getByRole("button", { name: "Back up now" })).toBeDisabled();
    expect(within(section).getByText(/the game creates its world when the server first starts/)).toBeInTheDocument();
    expect(within(section).getByText("Never")).toBeInTheDocument();
  });

  it("validates the autosave period and the backup schedule before saving, then saves only what changed", async () => {
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: pageRoutes(serverFixture(), [], {
        "PATCH /servers/{id}": () => jsonResponse(200, serverFixture({ backup_schedule: { interval_hours: 24, keep: 5 } })),
      }),
    });
    const section = await dataSection();
    const form = within(section).getByRole("form", { name: "Autosave and backup schedule" });
    const snapshot = within(form).getByLabelText("Autosave period (seconds)");
    const interval = within(form).getByLabelText("Back up every (hours)");
    const keep = within(form).getByLabelText("Scheduled backups to keep");
    expect(snapshot).toHaveValue("300");
    expect(within(form).getByText(/Applies when the server is restarted/)).toBeInTheDocument();
    await user.clear(snapshot);
    await user.type(snapshot, "5");
    await user.clear(interval);
    await user.type(interval, "721");
    await user.clear(keep);
    await user.type(keep, "0");
    await user.click(within(form).getByRole("button", { name: "Save settings" }));
    expect(within(form).getByText("Use 0 to turn autosave off, or 10 to 86,400 seconds (24 hours).")).toBeInTheDocument();
    expect(within(form).getByText("Use 0 to turn scheduled backups off, or 1 to 720 hours (30 days).")).toBeInTheDocument();
    expect(within(form).getByText("Keep 1 to 100 scheduled backups.")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);

    await user.clear(snapshot);
    await user.type(snapshot, "300");
    await user.clear(interval);
    await user.type(interval, "24");
    await user.clear(keep);
    await user.type(keep, "5");
    await user.click(within(form).getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ backup_schedule: { interval_hours: 24, keep: 5 } }));
    expect(await screen.findByText("Backup schedule saved; it applies now.")).toBeInTheDocument();
  });

  it("says the autosave period waits for a restart and routes the daemon's field errors", async () => {
    let n = 0;
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: pageRoutes(serverFixture(), [], {
        "PATCH /servers/{id}": () =>
          ++n === 1
            ? errorResponse(400, "validation_failed", "snapshot_interval_seconds must be 0 or 10 to 86400", { field: "snapshot_interval_seconds" })
            : jsonResponse(200, serverFixture({ snapshot_interval_seconds: 60 })),
      }),
    });
    const section = await dataSection();
    const form = within(section).getByRole("form", { name: "Autosave and backup schedule" });
    const snapshot = within(form).getByLabelText("Autosave period (seconds)");
    await user.clear(snapshot);
    await user.type(snapshot, "60");
    expect(within(form).getByText(/Saves every 1 minute\./)).toBeInTheDocument();
    await user.click(within(form).getByRole("button", { name: "Save settings" }));
    expect(await within(form).findByText("Snapshot_interval_seconds must be 0 or 10 to 86400.")).toBeInTheDocument();
    await user.click(within(form).getByRole("button", { name: "Save settings" }));
    expect(await screen.findByText("Autosave period saved; it applies when the server is restarted.")).toBeInTheDocument();
    expect(calls.filter((c) => c.method === "PATCH").map((c) => c.body)).toEqual([{ snapshot_interval_seconds: 60 }, { snapshot_interval_seconds: 60 }]);
  });
});

describe("restoring a backup", () => {
  const restoreJob = job({ id: 91, kind: "server.restore", subject_type: "server", subject_id: "3", status: "queued", restricted: true });
  const restoreRoutes = (handler: Handler): Record<string, Handler> => ({
    "POST /servers/{id}/restore": handler,
    "GET /jobs/{id}": () => jsonResponse(200, restoreJob),
  });

  it("says what happens and restores a backup of the same schema", async () => {
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [backupFixture()], restoreRoutes(() => jsonResponse(202, restoreJob))),
    });
    const section = await dataSection();
    await user.click(await within(section).findByRole("button", { name: "Restore backup 40" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup into echo" });
    expect(within(dialog).getByText(/every connected player is disconnected/)).toBeInTheDocument();
    expect(within(dialog).getByText(/Its current data is backed up first/)).toBeInTheDocument();
    expect(within(dialog).getByText("The backup's data replaces it.")).toBeInTheDocument();
    expect(within(dialog).getByText(/it starts again with the restored world/)).toBeInTheDocument();
    expect(within(dialog).queryByText(/cannot load this backup/)).toBeNull();
    await user.click(within(dialog).getByRole("button", { name: "Restore backup" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/servers/3/restore")?.body).toEqual({ backup_id: 40 }));
    const inline = await screen.findByLabelText("Job 91");
    expect(within(inline).getByText("Restore backup")).toBeInTheDocument();
  });

  it("asks for an explicit confirmation before forcing a restore across schemas", async () => {
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [backupFixture({ schema_fingerprint: OLD_FP })], restoreRoutes(() => jsonResponse(202, restoreJob))),
    });
    const section = await dataSection();
    await user.click(await within(section).findByRole("button", { name: "Restore backup 40" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup into echo" });
    expect(await within(dialog).findByText("The build of echo cannot load this backup")).toBeInTheDocument();
    expect(within(dialog).getByText(OLD_FP.slice(0, 12))).toBeInTheDocument();
    const go = within(dialog).getByRole("button", { name: "Restore anyway" });
    expect(go).toBeDisabled();
    await user.click(within(dialog).getByLabelText("Restore anyway: I will deploy a build that can load it"));
    expect(go).toBeEnabled();
    await user.click(go);
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/servers/3/restore")?.body).toEqual({ backup_id: 40, force: true }));
  });

  it("handles a 409 schema_mismatch the daemon detected by asking before forcing", async () => {
    let n = 0;
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [backupFixture()], {
        // The client does not know the build's fingerprint yet.
        "GET /builds": () => jsonResponse(200, { items: [buildFixture({ schema_fingerprint: "" })] }),
        ...restoreRoutes(() =>
          ++n === 1
            ? errorResponse(409, "schema_mismatch", "the backup was saved with another schema than build 12; restore with force to do it anyway")
            : jsonResponse(202, restoreJob),
        ),
      }),
    });
    const section = await dataSection();
    await user.click(await within(section).findByRole("button", { name: "Restore backup 40" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup into echo" });
    await user.click(within(dialog).getByRole("button", { name: "Restore backup" }));
    expect(await within(dialog).findByText("The backup was saved with another schema than build 12; restore with force to do it anyway.")).toBeInTheDocument();
    const go = within(dialog).getByRole("button", { name: "Restore anyway" });
    expect(go).toBeDisabled();
    await user.click(within(dialog).getByLabelText("Restore anyway: I will deploy a build that can load it"));
    await user.click(go);
    await waitFor(() =>
      expect(calls.filter((c) => c.method === "POST" && c.path === "/servers/3/restore").map((c) => c.body)).toEqual([
        { backup_id: 40 },
        { backup_id: 40, force: true },
      ]),
    );
    expect(await screen.findByLabelText("Job 91")).toBeInTheDocument();
  });

  it("offers skipping the safety backup only as a secondary option", async () => {
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [backupFixture()], restoreRoutes(() => jsonResponse(202, restoreJob))),
    });
    const section = await dataSection();
    await user.click(await within(section).findByRole("button", { name: "Restore backup 40" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup into echo" });
    await user.click(within(dialog).getByText("More options"));
    await user.click(within(dialog).getByLabelText("Skip the safety backup"));
    expect(within(dialog).getByText("Its current data is not backed up: it is discarded for good.")).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Restore backup" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.path === "/servers/3/restore")?.body).toEqual({ backup_id: 40, skip_safety_backup: true }),
    );
  });

  it("picks a backup of the same project by default, including deleted servers, and all projects on request", async () => {
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [], {
        "GET /backups": (req) =>
          jsonResponse(
            200,
            backupList(
              req.query.get("server_id")
                ? []
                : [
                    backupFixture({ id: 60, server_id: null, server_name: "old-echo", note: "season 1" }),
                    backupFixture({ id: 61, server_id: 4, server_name: "kart-eu", project_id: 6, project_name: "kart" }),
                  ],
            ),
          ),
      }),
    });
    const section = await dataSection();
    await user.click(within(section).getByRole("button", { name: "Restore from any backup" }));
    const picker = await screen.findByRole("dialog", { name: "Choose a backup to restore into echo" });
    const table = await within(picker).findByRole("table", { name: "Backups to restore" });
    expect(within(table).getAllByRole("row")).toHaveLength(2);
    expect(within(table).getByText("old-echo")).toBeInTheDocument();
    expect(within(table).getByText("Deleted server")).toBeInTheDocument();
    await user.selectOptions(within(picker).getByLabelText("Show"), "all");
    expect(within(picker).getAllByRole("row")).toHaveLength(3);
    await user.click(within(picker).getByRole("button", { name: "Restore backup 60" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup into echo" });
    expect(within(dialog).getByText(/Backup of old-echo taken/)).toBeInTheDocument();
  });
});

describe("resetting data and the schema warning", () => {
  it("requires typing the server name before resetting", async () => {
    const resetJob = job({ id: 92, kind: "server.data_reset", subject_type: "server", subject_id: "3", status: "queued", restricted: true });
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [], {
        "POST /servers/{id}/data/reset": () => jsonResponse(202, resetJob),
        "GET /jobs/{id}": () => jsonResponse(200, resetJob),
      }),
    });
    const section = await dataSection();
    await user.click(within(section).getByRole("button", { name: "Reset data" }));
    const dialog = await screen.findByRole("dialog", { name: "Reset the data of echo" });
    expect(within(dialog).getByText(/backed up first \(a backup of kind “before a reset”\), then discarded/)).toBeInTheDocument();
    expect(within(dialog).getByText(/starts again with an empty world/)).toBeInTheDocument();
    const go = within(dialog).getByRole("button", { name: "Reset data" });
    expect(go).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Type echo to confirm"), "ech");
    expect(go).toBeDisabled();
    await user.type(within(dialog).getByLabelText("Type echo to confirm"), "o");
    expect(go).toBeEnabled();
    await user.click(go);
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/servers/3/data/reset")?.body).toEqual({}));
    const inline = await screen.findByLabelText("Job 92");
    expect(within(inline).getByText("Reset data")).toBeInTheDocument();
  });

  it("warns that the server will not start and offers the three ways out to admins", async () => {
    const mismatched = serverFixture({ data: { volume: "golem-deploy-data-3-0001", schema_fingerprint: FINGERPRINT, schema_mismatch: true, last_backup_at: null } });
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(mismatched, [], { "GET /builds": () => jsonResponse(200, { items: [buildFixture({ schema_fingerprint: OLD_FP })] }) }),
    });
    const title = await screen.findByText("This server will not start with its saved world");
    const box = title.closest("[role=alert]") as HTMLElement;
    expect(within(box).getByText(/cannot load a world saved with other schemas/)).toBeInTheDocument();
    expect(await within(box).findByText(OLD_FP.slice(0, 12))).toBeInTheDocument();
    expect(within(box).getByRole("button", { name: "Deploy another build" })).toBeInTheDocument();
    expect(within(box).getByRole("button", { name: "Restore a backup" })).toBeInTheDocument();
    await user.click(within(box).getByRole("button", { name: "Reset data" }));
    expect(await screen.findByRole("dialog", { name: "Reset the data of echo" })).toBeInTheDocument();
  });

  it("shows developers the warning without the admin actions", async () => {
    const mismatched = serverFixture({ data: { volume: "v", schema_fingerprint: FINGERPRINT, schema_mismatch: true, last_backup_at: null } });
    renderApp({ path: "/servers/3", signedInAs: developer, routes: pageRoutes(mismatched, []) });
    const box = (await screen.findByText("This server will not start with its saved world")).closest("[role=alert]") as HTMLElement;
    expect(within(box).getByRole("button", { name: "Deploy another build" })).toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Restore a backup" })).toBeNull();
    expect(within(box).queryByRole("button", { name: "Reset data" })).toBeNull();
    expect(within(box).getByText("Restoring a backup and resetting the data need an administrator.")).toBeInTheDocument();
  });

  it("warns before assigning a build that cannot load the saved world, without blocking it", async () => {
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: pageRoutes(serverFixture(), [], {
        "GET /builds": () =>
          jsonResponse(200, { items: [buildFixture({ id: 14, commit: OTHER_COMMIT, schema_fingerprint: OLD_FP }), buildFixture({ id: 12, schema_fingerprint: FINGERPRINT })] }),
        "PATCH /servers/{id}": () => jsonResponse(200, serverFixture({ build_id: 14 })),
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Deploy another build" }));
    const dialog = await screen.findByRole("dialog", { name: "Deploy a build to echo" });
    expect(await within(dialog).findByText("This build cannot load the saved world")).toBeInTheDocument();
    expect(within(dialog).getByText(/the server will not start \(starts and restarts fail\)/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Assign without restarting" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ build_id: 14 }));
  });

  it("says in the delete confirmation that a last backup is stored and kept, then the world goes", async () => {
    const { user } = renderApp({ path: "/servers/3", signedInAs: account(), routes: pageRoutes(serverFixture(), []) });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete server" });
    expect(
      within(confirm).getByText(/A last backup of its saved world is stored first \(a backup of kind “before a deletion”\) and kept on the Backups page; then the world is deleted with the server\./),
    ).toBeInTheDocument();
    expect(within(confirm).getByText(/Its backups are kept/)).toBeInTheDocument();
  });

  it("downloads a backup with the bearer header under the daemon's file name", async () => {
    const names: string[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      names.push(this.download);
    });
    const urls = { create: URL.createObjectURL, revoke: URL.revokeObjectURL };
    URL.createObjectURL = () => "blob:backup";
    URL.revokeObjectURL = () => {};
    try {
      const { user, calls } = renderApp({
        path: "/servers/3",
        signedInAs: developer,
        routes: pageRoutes(serverFixture(), [backupFixture()], {
          "GET /backups/{id}/download": () =>
            new Response(new Blob([new Uint8Array([0x1f, 0x8b])]), {
              status: 200,
              headers: { "Content-Type": "application/gzip", "Content-Disposition": 'attachment; filename="echo-backup-40.tar.gz"' },
            }),
        }),
      });
      await user.click(await screen.findByRole("button", { name: "Download backup 40" }));
      await waitFor(() => expect(names).toEqual(["echo-backup-40.tar.gz"]));
      expect(calls.find((c) => c.path === "/backups/40/download")?.headers.Authorization).toMatch(/^Bearer /);
    } finally {
      click.mockRestore();
      URL.createObjectURL = urls.create;
      URL.revokeObjectURL = urls.revoke;
    }
  });
});

describe("backups page", () => {
  const items = [
    backupFixture({ id: 52, server_id: null, server_name: "old-arena", note: "season 1", size_bytes: 1024 * 1024 }),
    backupFixture({ id: 51, server_id: 4, server_name: "kart-eu", project_id: 6, project_name: "kart", size_bytes: 1024 * 1024 }),
    backupFixture({ id: 50, size_bytes: 1024 * 1024 }),
  ];
  const routes = (extra: Record<string, Handler> = {}): Record<string, Handler> => ({
    "GET /backups": () => jsonResponse(200, backupList(items, { total_bytes: 5 * 1024 * 1024 })),
    "GET /servers": () =>
      jsonResponse(200, { items: [serverFixture(), serverFixture({ id: 4, name: "kart-eu", project_id: 6, build_id: 13, port: 7811 })] }),
    "GET /builds": () => jsonResponse(200, { items: [buildFixture({ schema_fingerprint: FINGERPRINT }), buildFixture({ id: 13, project_id: 6 })] }),
    ...extra,
  });

  it("lists every backup with the total size and filters by server, deleted servers included", async () => {
    const { user } = renderApp({ path: "/backups", signedInAs: developer, routes: routes() });
    const table = await screen.findByRole("table", { name: "Backups" });
    expect(within(table).getAllByRole("row")).toHaveLength(4);
    expect(screen.getByText(/3 backups, 5\.0 MiB stored in total/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Backups" })).toHaveAttribute("href", "/backups");
    const filter = screen.getByLabelText("Server");
    expect(within(filter).getAllByRole("option").map((o) => o.textContent)).toEqual(["All servers", "echo", "kart-eu", "old-arena (deleted)"]);
    await user.selectOptions(filter, "old-arena (deleted)");
    const rows = within(screen.getByRole("table", { name: "Backups" })).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).getByText("Deleted server")).toBeInTheDocument();
    expect(within(rows[0]).getByText("season 1")).toBeInTheDocument();
    await user.selectOptions(filter, "kart-eu");
    expect(within(screen.getByRole("table", { name: "Backups" })).getByRole("link", { name: "kart-eu" })).toHaveAttribute("href", "/servers/4");
    // Developers download; restore and delete need an administrator.
    expect(screen.queryByRole("button", { name: "Restore backup 51" })).toBeNull();
    expect(screen.getByRole("button", { name: "Download backup 51" })).toBeInTheDocument();
  });

  it("asks which server to restore into and follows the job there", async () => {
    const restoreJob = job({ id: 93, kind: "server.restore", subject_type: "server", subject_id: "4", status: "queued", restricted: true });
    const { user, calls } = renderApp({
      path: "/backups",
      signedInAs: account(),
      routes: routes({
        "POST /servers/{id}/restore": () => jsonResponse(202, restoreJob),
        "GET /jobs/{id}": () => jsonResponse(200, restoreJob),
        "GET /jobs/{id}/log": () => streamResponse().response,
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Restore backup 52" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup" });
    // A deleted server's backup goes to a server of the same project by default.
    const target = await within(dialog).findByLabelText("Restore into");
    expect(target).toHaveValue("3");
    await user.selectOptions(target, "4");
    await user.click(within(dialog).getByRole("button", { name: "Restore backup" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/servers/4/restore")?.body).toEqual({ backup_id: 52 }));
    expect(await screen.findByLabelText("Job 93")).toBeInTheDocument();
    expect(screen.getByText(/Restoring into/).querySelector("a")).toHaveAttribute("href", "/servers/4");
  });

  it("lets an admin delete a backup and shows a 409 as worded", async () => {
    const { user } = renderApp({
      path: "/backups",
      signedInAs: account(),
      routes: routes({ "DELETE /backups/{id}": () => errorResponse(409, "conflict", "a restore is reading this backup; wait for it to finish") }),
    });
    await user.click(await screen.findByRole("button", { name: "Delete backup 50" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete backup" });
    await user.click(within(confirm).getByRole("button", { name: "Delete backup" }));
    expect(await within(confirm).findByText("A restore is reading this backup; wait for it to finish.")).toBeInTheDocument();
  });
});

describe("volumes page", () => {
  it("marks current data volumes and offers deletion only for leftovers", async () => {
    const volume = (overrides: Partial<Volume>): Volume => ({
      name: "golem-deploy-data-3-0001",
      server_id: 3,
      server_name: "echo",
      current: true,
      created_at: "2026-11-20T10:00:00Z",
      in_use: true,
      ...overrides,
    });
    renderApp({
      path: "/volumes",
      signedInAs: account(),
      routes: {
        "GET /volumes": () => jsonResponse(200, { items: [volume({}), volume({ name: "golem-deploy-data-3-0000", current: false, in_use: false })] }),
      },
    });
    const table = await screen.findByRole("table", { name: "Volumes" });
    expect(within(table).getByText("Current data")).toBeInTheDocument();
    expect(within(table).getByText("Leftover")).toBeInTheDocument();
    expect(within(table).getByRole("button", { name: "Delete volume golem-deploy-data-3-0000" })).toBeInTheDocument();
    expect(within(table).queryByRole("button", { name: "Delete volume golem-deploy-data-3-0001" })).toBeNull();
  });
});

describe("saved data rules mirrored in the client", () => {
  it("labels the new job kinds", () => {
    expect(jobKindLabel("server.backup")).toBe("Back up data");
    expect(jobKindLabel("server.restore")).toBe("Restore backup");
    expect(jobKindLabel("server.data_reset")).toBe("Reset data");
  });

  it("compares fingerprints only when both are known", () => {
    expect(schemaMatch("a", "a")).toBe("match");
    expect(schemaMatch("a", "b")).toBe("differs");
    expect(schemaMatch("", "b")).toBe("unknown");
    expect(schemaMatch("a", "")).toBe("unknown");
  });

  it("checks the settings' ranges like the daemon", () => {
    expect(validateSnapshotInterval("0")).toBeNull();
    expect(validateSnapshotInterval("10")).toBeNull();
    expect(validateSnapshotInterval("86400")).toBeNull();
    expect(validateSnapshotInterval("9")).not.toBeNull();
    expect(validateSnapshotInterval("86401")).not.toBeNull();
    expect(validateSnapshotInterval("1.5")).not.toBeNull();
    expect(validateBackupInterval("0")).toBeNull();
    expect(validateBackupInterval("720")).toBeNull();
    expect(validateBackupInterval("721")).not.toBeNull();
    expect(validateBackupInterval("-1")).not.toBeNull();
    expect(validateBackupKeep("1")).toBeNull();
    expect(validateBackupKeep("100")).toBeNull();
    expect(validateBackupKeep("0")).not.toBeNull();
    expect(validateBackupKeep("101")).not.toBeNull();
    expect(validateBackupNote("x".repeat(200))).toBeNull();
    expect(validateBackupNote("x".repeat(201))).not.toBeNull();
    expect(validateBackupNote("a\nb")).not.toBeNull();
  });

  it("accepts the optional settings in import documents and rejects out-of-range ones", () => {
    const doc = (spec: Record<string, unknown>) => JSON.stringify({ format_version: 2, servers: [{ name: "a", project: "arena", ...spec }] });
    expect(checkServerImport(doc({ snapshot_interval_seconds: 60, backup_schedule: { interval_hours: 24, keep: 7 } })).errors).toEqual([]);
    expect(checkServerImport(doc({ snapshot_interval_seconds: 5 })).errors).toEqual(["Server 1 (a): snapshot_interval_seconds must be 0 or 10 to 86400."]);
    expect(checkServerImport(doc({ backup_schedule: { interval_hours: 721, keep: 0 } })).errors).toEqual([
      "Server 1 (a): backup_schedule.interval_hours must be 0 (off) or 1 to 720.",
      "Server 1 (a): backup_schedule.keep must be 1 to 100.",
    ]);
  });

  it("lists existing servers before deleted ones in the backups filter", () => {
    expect(
      serverFilterOptions([
        backupFixture({ id: 3, server_id: null, server_name: "gone" }),
        backupFixture({ id: 2, server_id: 9, server_name: "zeta" }),
        backupFixture({ id: 1, server_id: 3, server_name: "alpha" }),
        backupFixture({ id: 0, server_id: 3, server_name: "alpha-old-name" }),
      ]),
    ).toEqual([
      { value: "server:3", label: "alpha" },
      { value: "server:9", label: "zeta" },
      { value: "deleted:gone", label: "gone (deleted)" },
    ]);
  });
});

describe("incomplete and pre-delete backups", () => {
  it("labels backups taken before a deletion and says how many entries a backup leaves out", async () => {
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [
        backupFixture({ id: 44, kind: "pre_delete", omitted_entries: 3, note: "" }),
        backupFixture({ id: 43, omitted_entries: 1, live: false, note: "" }),
        backupFixture({ id: 40 }),
      ]),
    });
    const section = await dataSection();
    const table = await within(section).findByRole("table", { name: "Backups of this server" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(within(rows[0]).getByText("Before a deletion")).toBeInTheDocument();
    expect(within(rows[0]).getByText("3 entries left out")).toHaveAttribute("title", OMITTED_HINT);
    expect(within(rows[1]).getByText("1 entry left out")).toBeInTheDocument();
    // Files copied without a fresh save: a short second line, with the explanation in its title.
    expect(within(rows[1]).getByText("No fresh save")).toHaveAttribute("title", expect.stringContaining("copied as they were"));
    expect(within(rows[2]).queryByText(/left out/)).toBeNull();
    expect(backupKindLabel("pre_delete")).toBe("Before a deletion");

    await user.click(within(section).getByRole("button", { name: "Restore backup 44" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup into echo" });
    expect(within(dialog).getByText("This backup is incomplete: 3 entries left out")).toBeInTheDocument();
    expect(within(dialog).getByText(OMITTED_HINT)).toBeInTheDocument();
    expect(within(dialog).getByText(/Backup of echo taken .* \(before a deletion,/)).toBeInTheDocument();
  });

  it("keeps the taken and kind values on one line so that they do not wrap word by word", async () => {
    renderApp({ path: "/backups", signedInAs: developer, routes: { "GET /backups": () => jsonResponse(200, backupList([backupFixture({ kind: "pre_restore" })])) } });
    const table = await screen.findByRole("table", { name: "Backups" });
    expect(within(table).getByText("Before a restore").parentElement).toHaveClass("whitespace-nowrap");
    expect(within(table).getByText("by alice").parentElement).toHaveClass("whitespace-nowrap");
  });

  it("warns before downloading a large backup in the browser and shows the curl alternative", async () => {
    const names: string[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      names.push(this.download);
    });
    const urls = { create: URL.createObjectURL, revoke: URL.revokeObjectURL };
    URL.createObjectURL = () => "blob:backup";
    URL.revokeObjectURL = () => {};
    try {
      const { user, calls } = renderApp({
        path: "/servers/3",
        signedInAs: developer,
        routes: pageRoutes(serverFixture(), [backupFixture({ size_bytes: 600 * 1024 * 1024 })], {
          "GET /backups/{id}/download": () =>
            new Response(new Blob([new Uint8Array([0x1f, 0x8b])]), {
              status: 200,
              headers: { "Content-Type": "application/gzip", "Content-Disposition": 'attachment; filename="echo-backup-40.tar.gz"' },
            }),
        }),
      });
      await user.click(await screen.findByRole("button", { name: "Download backup 40" }));
      const confirm = await screen.findByRole("alertdialog", { name: "Download a large backup" });
      expect(within(confirm).getByText(/This archive is 600 MiB\..*may fail in the browser/)).toBeInTheDocument();
      expect(within(confirm).getByText(/curl -fL -H "Authorization: Bearer \$TOKEN" .*\/api\/v1\/backups\/40\/download/)).toBeInTheDocument();
      expect(calls.some((c) => c.path === "/backups/40/download")).toBe(false);
      await user.click(within(confirm).getByRole("button", { name: "Download in the browser" }));
      await waitFor(() => expect(names).toEqual(["echo-backup-40.tar.gz"]));
    } finally {
      click.mockRestore();
      URL.createObjectURL = urls.create;
      URL.revokeObjectURL = urls.revoke;
    }
  });
});

describe("restore dialog consent", () => {
  const OTHER_FP = "b".repeat(16);
  const twoServers = [serverFixture(), serverFixture({ id: 4, name: "kart-eu", project_id: 6, build_id: 13, port: 7811 })];
  const builds = [buildFixture({ schema_fingerprint: FINGERPRINT }), buildFixture({ id: 13, project_id: 6, schema_fingerprint: OTHER_FP })];

  it("clears the force and skip ticks when the admin picks another server", async () => {
    const { user, calls } = renderApp({
      path: "/backups",
      signedInAs: account(),
      routes: {
        "GET /backups": () => jsonResponse(200, backupList([backupFixture({ id: 52, schema_fingerprint: OLD_FP })])),
        "GET /servers": () => jsonResponse(200, { items: twoServers }),
        "GET /builds": () => jsonResponse(200, { items: builds }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Restore backup 52" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup" });
    const target = await within(dialog).findByLabelText("Restore into");
    expect(target).toHaveValue("3");
    await within(dialog).findByText("The build of echo cannot load this backup");
    await user.click(within(dialog).getByLabelText("Restore anyway: I will deploy a build that can load it"));
    await user.click(within(dialog).getByText("More options"));
    await user.click(within(dialog).getByLabelText("Skip the safety backup"));
    expect(within(dialog).getByRole("button", { name: "Restore anyway" })).toBeEnabled();

    await user.selectOptions(target, "4");
    expect(await within(dialog).findByText("The build of kart-eu cannot load this backup")).toBeInTheDocument();
    expect(within(dialog).getByLabelText("Restore anyway: I will deploy a build that can load it")).not.toBeChecked();
    expect(within(dialog).getByLabelText("Skip the safety backup")).not.toBeChecked();
    expect(within(dialog).getByRole("button", { name: "Restore anyway" })).toBeDisabled();

    // Back to the first server: the earlier ticks do not come back either.
    await user.selectOptions(target, "3");
    expect(within(dialog).getByLabelText("Restore anyway: I will deploy a build that can load it")).not.toBeChecked();
    expect(within(dialog).getByLabelText("Skip the safety backup")).not.toBeChecked();
    expect(calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("says the server stays stopped when the schemas are known to differ", async () => {
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [backupFixture({ schema_fingerprint: OLD_FP })]),
    });
    const section = await dataSection();
    await user.click(await within(section).findByRole("button", { name: "Restore backup 40" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup into echo" });
    expect(await within(dialog).findByText("The server stays stopped until a build with the backup's schema is assigned to it.")).toBeInTheDocument();
    expect(within(dialog).queryByText(/starts again/)).toBeNull();
  });

  it("clears the ticks when the target's build fingerprint changes while the dialog is open", async () => {
    let buildFp = FINGERPRINT;
    const { user, services } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [backupFixture({ schema_fingerprint: OLD_FP })], {
        "GET /builds": () => jsonResponse(200, { items: [buildFixture({ schema_fingerprint: buildFp })] }),
      }),
    });
    const section = await dataSection();
    await user.click(await within(section).findByRole("button", { name: "Restore backup 40" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup into echo" });
    await within(dialog).findByText("The build of echo cannot load this backup");
    await user.click(within(dialog).getByLabelText("Restore anyway: I will deploy a build that can load it"));
    await user.click(within(dialog).getByText("More options"));
    await user.click(within(dialog).getByLabelText("Skip the safety backup"));
    expect(within(dialog).getByRole("button", { name: "Restore anyway" })).toBeEnabled();

    buildFp = OTHER_FP;
    await act(() => services.queryClient.invalidateQueries());
    await waitFor(() => expect(within(dialog).getByText(OTHER_FP.slice(0, 12))).toBeInTheDocument());
    expect(within(dialog).getByLabelText("Restore anyway: I will deploy a build that can load it")).not.toBeChecked();
    expect(within(dialog).getByLabelText("Skip the safety backup")).not.toBeChecked();
    expect(within(dialog).getByRole("button", { name: "Restore anyway" })).toBeDisabled();
  });

  it("requires choosing again when the chosen server disappears, without falling back to another one", async () => {
    let servers = twoServers;
    const { user, services, calls } = renderApp({
      path: "/backups",
      signedInAs: account(),
      routes: {
        "GET /backups": () => jsonResponse(200, backupList([backupFixture({ id: 52, schema_fingerprint: "" })])),
        "GET /servers": () => jsonResponse(200, { items: servers }),
        "GET /builds": () => jsonResponse(200, { items: builds }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Restore backup 52" }));
    const dialog = await screen.findByRole("dialog", { name: "Restore a backup" });
    const target = await within(dialog).findByLabelText("Restore into");
    expect(target).toHaveValue("3");
    await user.click(within(dialog).getByText("More options"));
    await user.click(within(dialog).getByLabelText("Skip the safety backup"));

    servers = [twoServers[1]];
    await act(() => services.queryClient.invalidateQueries({ queryKey: ["servers"] }));
    expect(await within(dialog).findByText("The chosen server is no longer listed")).toBeInTheDocument();
    expect(target).toHaveValue("");
    expect(within(dialog).queryByLabelText("Skip the safety backup")).toBeNull();
    expect(within(dialog).getByRole("button", { name: "Restore backup" })).toBeDisabled();

    await user.selectOptions(target, "4");
    expect(within(dialog).queryByText("The chosen server is no longer listed")).toBeNull();
    expect(within(dialog).getByLabelText("Skip the safety backup")).not.toBeChecked();
    await user.click(within(dialog).getByRole("button", { name: "Restore backup" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.path).toBe("/servers/4/restore"));
  });
});

describe("deleting a server", () => {
  const refusal = "the last backup of the saved data would leave 2 entries of the data directory out; nothing was deleted. An administrator can delete the server anyway with skip_safety_backup=1";
  const failedDelete = job({ id: 61, kind: "server.delete", subject_type: "server", subject_id: "3", status: "error", detail: refusal, finished_at: "2026-11-24T10:00:00Z" });

  it("lets an admin skip the last backup as a secondary option", async () => {
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [], { "DELETE /servers/{id}": () => jsonResponse(204, null), "GET /servers": () => jsonResponse(200, { items: [] }) }),
    });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete server" });
    await user.click(within(confirm).getByText("More options"));
    await user.click(within(confirm).getByLabelText("Skip the last backup"));
    expect(within(confirm).getByText(/Its saved world is not backed up: it is deleted with the server and cannot be brought back\./)).toBeInTheDocument();
    await user.click(within(confirm).getByRole("button", { name: "Delete server" }));
    await waitFor(() => expect(calls.find((c) => c.method === "DELETE")?.query.get("skip_safety_backup")).toBe("1"));
    expect(await screen.findByText(/Server echo deleted: .* Its backups are kept\./)).toBeInTheDocument();
  });

  it("offers developers no way to skip the last backup and sends none", async () => {
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: pageRoutes(serverFixture(), [], { "DELETE /servers/{id}": () => jsonResponse(204, null), "GET /servers": () => jsonResponse(200, { items: [] }) }),
    });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete server" });
    expect(within(confirm).queryByText("More options")).toBeNull();
    expect(within(confirm).queryByLabelText("Skip the last backup")).toBeNull();
    await user.click(within(confirm).getByRole("button", { name: "Delete server" }));
    await waitFor(() => expect(calls.find((c) => c.method === "DELETE")?.path).toBe("/servers/3"));
    expect(calls.find((c) => c.method === "DELETE")?.query.has("skip_safety_backup")).toBe(false);
  });

  it("keeps the dialog open on a refused last backup, shows the daemon's message and the admin's way out", async () => {
    let n = 0;
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: pageRoutes(serverFixture(), [], {
        "DELETE /servers/{id}": () => (++n === 1 ? errorResponse(409, "conflict", refusal) : jsonResponse(204, null)),
        "GET /jobs": (req) => jsonResponse(200, { items: req.query.get("limit") === "5" ? [failedDelete] : [] }),
        "GET /jobs/{id}": () => jsonResponse(200, failedDelete),
        "GET /servers": () => jsonResponse(200, { items: [] }),
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete server" });
    await user.click(within(confirm).getByRole("button", { name: "Delete server" }));
    expect(
      await within(confirm).findByText(
        "The last backup of the saved data would leave 2 entries of the data directory out; nothing was deleted. An administrator can delete the server anyway with skip_safety_backup=1.",
      ),
    ).toBeInTheDocument();
    expect(within(confirm).getByText(/To delete the server anyway, tick “Skip the last backup” under More options/)).toBeInTheDocument();
    // The failed job is shown on the page as before.
    expect(await screen.findByLabelText("Job 61")).toBeInTheDocument();
    await user.click(within(confirm).getByLabelText("Skip the last backup"));
    await user.click(within(confirm).getByRole("button", { name: "Delete server" }));
    await waitFor(() => expect(calls.filter((c) => c.method === "DELETE").map((c) => c.query.get("skip_safety_backup"))).toEqual([null, "1"]));
  });

  it("tells a developer that an administrator can delete without the last backup", async () => {
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: pageRoutes(serverFixture(), [], { "DELETE /servers/{id}": () => errorResponse(409, "conflict", refusal) }),
    });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete server" });
    await user.click(within(confirm).getByRole("button", { name: "Delete server" }));
    expect(await within(confirm).findByText(/An administrator can delete the server without the last backup\./)).toBeInTheDocument();
    expect(within(confirm).queryByLabelText("Skip the last backup")).toBeNull();
  });
});

describe("notes and durations", () => {
  const cp = (n: number) => String.fromCodePoint(n);

  it("refuses the characters the daemon refuses in a note and trims surrounding space like it", () => {
    for (const bad of [0x00, 0x09, 0x1b, 0x7f, 0x85, 0x9f, 0x202a, 0x202e, 0x2066, 0x2069]) {
      expect(validateBackupNote(`a${cp(bad)}b`), `U+${bad.toString(16)}`).not.toBeNull();
    }
    expect(validateBackupNote(`a${cp(0x2028)}b`)).toBe("Use a single line.");
    expect(validateBackupNote(`a${cp(0x2029)}b`)).toBe("Use a single line.");
    expect(validateBackupNote(`a${cp(0x200b)}b ${cp(0xe9)}`)).toBeNull();
    // Surrounding space is removed first, as Go's strings.TrimSpace does, also a trailing newline.
    expect(trimBackupNote(`${cp(0xa0)} \tnote${cp(0x3000)}\n`)).toBe("note");
    expect(validateBackupNote(` ${"x".repeat(200)} \n`)).toBeNull();
  });

  it("checks the note before sending it", async () => {
    const { user, calls } = renderApp({ path: "/servers/3", signedInAs: developer, routes: pageRoutes(serverFixture(), []) });
    const section = await dataSection();
    await user.type(within(section).getByLabelText("Note (optional)"), `left${cp(0x202e)}right`);
    await user.click(within(section).getByRole("button", { name: "Back up now" }));
    expect(within(section).getByText("Remove the control characters and text direction marks.")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("writes durations in words without zero parts", () => {
    expect(formatDurationWords(300)).toBe("5 minutes");
    expect(formatDurationWords(90)).toBe("90 seconds");
    expect(formatDurationWords(5400)).toBe("1 hour 30 minutes");
    expect(formatDurationWords(60)).toBe("1 minute");
    expect(formatDurationWords(10)).toBe("10 seconds");
    expect(formatDurationWords(150)).toBe("2 minutes 30 seconds");
    expect(formatDurationWords(86400)).toBe("1 day");
    expect(formatDurationWords(3601)).toBe("1 hour 1 second");
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});
