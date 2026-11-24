package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustAccount(t *testing.T, s *Store, name string, role Role, approved bool) Account {
	t.Helper()
	a, err := s.CreateAccount(context.Background(), NewAccount{Username: name, PasswordHash: "hash", Role: role, Approved: approved})
	if err != nil {
		t.Fatalf("create account %s: %v", name, err)
	}
	return a
}

func TestMigrationsApplyAndAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil || v < 1 {
		t.Fatalf("schema version %d, err %v", v, err)
	}
	// Running the migrations again is a no-op.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	mustAccount(t, s, "alice", RoleAdmin, true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetAccountByUsername(ctx, "alice"); err != nil {
		t.Fatalf("data lost after reopen: %v", err)
	}
	migs, err := loadMigrations(migrationFS)
	if err != nil || migs[len(migs)-1].version != v {
		t.Fatalf("latest migration vs schema version %d: %v", v, err)
	}
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(migs) {
		t.Fatalf("schema_migrations rows %d (err %v), want %d", n, err, len(migs))
	}
	// Pragmas are applied on every connection.
	var fk int
	if err := s2.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, err %v", fk, err)
	}
	var mode string
	if err := s2.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, err %v", mode, err)
	}
}

func TestMigrationRunnerOrderingAndFailure(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	extra := fstest.MapFS{
		"migrations/0001_init.sql":   {Data: []byte("SELECT 1;")},
		"migrations/9001_add.sql":    {Data: []byte("CREATE TABLE extra_a (id INTEGER PRIMARY KEY);")},
		"migrations/9002_broken.sql": {Data: []byte("CREATE TABLE extra_b (id INTEGER PRIMARY KEY); THIS IS NOT SQL;")},
	}
	if err := s.migrateFS(ctx, extra); err == nil {
		t.Fatal("broken migration should fail")
	}
	// 9001 applied, 9002 rolled back entirely.
	if _, err := s.db.Exec(`INSERT INTO extra_a DEFAULT VALUES`); err != nil {
		t.Fatalf("9001 not applied: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO extra_b DEFAULT VALUES`); err == nil {
		t.Fatal("9002 partially applied")
	}
	if v, _ := s.SchemaVersion(ctx); v != 9001 {
		t.Fatalf("version %d, want 9001", v)
	}
	// The binary's own migrations are older than the database now.
	if err := s.migrate(ctx); err == nil {
		t.Fatal("expected refusal of a newer schema")
	}

	bad := fstest.MapFS{"migrations/1_x.sql": {Data: []byte("SELECT 1;")}}
	if _, err := loadMigrations(bad); err == nil {
		t.Fatal("expected bad name rejection")
	}
}

func TestAccountsCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	a := mustAccount(t, s, "alice", RoleAdmin, true)
	if a.ID == 0 || a.TokenVersion < 0 || a.CreatedAt.IsZero() {
		t.Fatalf("unexpected account %+v", a)
	}
	if _, err := s.CreateAccount(ctx, NewAccount{Username: "ALICE", PasswordHash: "x", Role: RoleDeveloper}); !errors.Is(err, ErrConflict) {
		t.Fatalf("case-insensitive duplicate: got %v", err)
	}
	if _, err := s.CreateAccount(ctx, NewAccount{Username: "bob", PasswordHash: "x", Role: "root"}); err == nil {
		t.Fatal("invalid role accepted")
	}
	got, err := s.GetAccountByUsername(ctx, "Alice")
	if err != nil || got.ID != a.ID {
		t.Fatalf("lookup by username: %v %+v", err, got)
	}
	if _, err := s.GetAccount(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	upd, revoked, err := s.SetPassword(ctx, a.ID, "newhash", false)
	if err != nil || upd.PasswordHash != "newhash" || upd.TokenVersion != a.TokenVersion+1 || revoked != 0 {
		t.Fatalf("set password: %v %+v", err, upd)
	}
	if err := s.BumpTokenVersion(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetAccount(ctx, a.ID); got.TokenVersion != a.TokenVersion+2 {
		t.Fatalf("token version %d, want %d", got.TokenVersion, a.TokenVersion+2)
	}
	mustAccount(t, s, "bob", RoleDeveloper, false)
	c, err := s.CountAccounts(ctx)
	if err != nil || c.Total != 2 || c.ApprovedAdmins != 1 || c.Pending != 1 {
		t.Fatalf("counts %+v err %v", c, err)
	}
	list, err := s.ListAccounts(ctx)
	if err != nil || len(list) != 2 || list[0].Username != "alice" {
		t.Fatalf("list %+v err %v", list, err)
	}
}

func TestAccountInvariants(t *testing.T) {
	ctx := context.Background()
	admin, dev := RoleAdmin, RoleDeveloper
	yes, no := true, false

	type setup struct {
		s         *Store
		a1, a2, d Account
	}
	newSetup := func(t *testing.T, twoAdmins bool) setup {
		s := openTest(t)
		su := setup{s: s, a1: mustAccount(t, s, "admin1", RoleAdmin, true), d: mustAccount(t, s, "dev", RoleDeveloper, true)}
		if twoAdmins {
			su.a2 = mustAccount(t, s, "admin2", RoleAdmin, true)
		}
		return su
	}

	tests := []struct {
		name      string
		twoAdmins bool
		run       func(su setup) error
		want      error
	}{
		{"demote last admin", false, func(su setup) error {
			_, err := su.s.UpdateAccount(ctx, 0, su.a1.ID, AccountUpdate{Role: &dev})
			return err
		}, ErrLastAdmin},
		{"unapprove last admin", false, func(su setup) error {
			_, err := su.s.UpdateAccount(ctx, 0, su.a1.ID, AccountUpdate{Approved: &no})
			return err
		}, ErrLastAdmin},
		{"delete last admin", false, func(su setup) error { return su.s.DeleteAccount(ctx, su.d.ID, su.a1.ID) }, ErrLastAdmin},
		{"self delete", true, func(su setup) error { return su.s.DeleteAccount(ctx, su.a1.ID, su.a1.ID) }, ErrSelf},
		{"self demote", true, func(su setup) error {
			_, err := su.s.UpdateAccount(ctx, su.a1.ID, su.a1.ID, AccountUpdate{Role: &dev})
			return err
		}, ErrSelf},
		{"self unapprove", true, func(su setup) error {
			_, err := su.s.UpdateAccount(ctx, su.a1.ID, su.a1.ID, AccountUpdate{Approved: &no})
			return err
		}, ErrSelf},
		{"self no-op update allowed", true, func(su setup) error {
			_, err := su.s.UpdateAccount(ctx, su.a1.ID, su.a1.ID, AccountUpdate{Role: &admin, Approved: &yes})
			return err
		}, nil},
		{"demote other admin", true, func(su setup) error {
			_, err := su.s.UpdateAccount(ctx, su.a1.ID, su.a2.ID, AccountUpdate{Role: &dev})
			return err
		}, nil},
		{"delete other admin", true, func(su setup) error { return su.s.DeleteAccount(ctx, su.a1.ID, su.a2.ID) }, nil},
		{"delete developer", false, func(su setup) error { return su.s.DeleteAccount(ctx, su.a1.ID, su.d.ID) }, nil},
		{"promote developer", false, func(su setup) error {
			_, err := su.s.UpdateAccount(ctx, su.a1.ID, su.d.ID, AccountUpdate{Role: &admin})
			return err
		}, nil},
		{"update missing", false, func(su setup) error {
			_, err := su.s.UpdateAccount(ctx, su.a1.ID, 999, AccountUpdate{Role: &admin})
			return err
		}, ErrNotFound},
		{"delete missing", false, func(su setup) error { return su.s.DeleteAccount(ctx, su.a1.ID, 999) }, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			su := newSetup(t, tt.twoAdmins)
			err := tt.run(su)
			if tt.want == nil && err != nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if c, _ := su.s.CountAccounts(ctx); c.ApprovedAdmins < 1 {
				t.Fatal("no approved admin left")
			}
		})
	}
}

func TestAccountUpdateBumpsTokenVersion(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustAccount(t, s, "admin1", RoleAdmin, true)
	d := mustAccount(t, s, "dev", RoleDeveloper, true)
	admin, no, yes := RoleAdmin, false, true

	v0 := d.TokenVersion
	a, err := s.UpdateAccount(ctx, 0, d.ID, AccountUpdate{Role: &admin})
	if err != nil || a.TokenVersion != v0+1 {
		t.Fatalf("role change: %v ver %d", err, a.TokenVersion)
	}
	a, err = s.UpdateAccount(ctx, 0, d.ID, AccountUpdate{Approved: &no})
	if err != nil || a.TokenVersion != v0+2 || a.Approved {
		t.Fatalf("unapprove: %v %+v", err, a)
	}
	a, err = s.UpdateAccount(ctx, 0, d.ID, AccountUpdate{Approved: &yes})
	if err != nil || a.TokenVersion != v0+2 || !a.Approved {
		t.Fatalf("approve should not bump: %v %+v", err, a)
	}
}

func TestDeleteAccountCascadesAndNullsJobs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	admin := mustAccount(t, s, "admin1", RoleAdmin, true)
	d := mustAccount(t, s, "dev", RoleDeveloper, true)
	if _, err := s.CreateAPIToken(ctx, NewAPIToken{AccountID: d.ID, Name: "ci", TokenHash: "h1", Prefix: "gdp_1"}); err != nil {
		t.Fatal(err)
	}
	j, err := s.CreateJob(ctx, NewJob{Kind: "k", SubjectType: "project", SubjectID: "1", CreatedBy: &d.ID})
	if err != nil {
		t.Fatal(err)
	}
	if j.CreatedByUsername == nil || *j.CreatedByUsername != "dev" {
		t.Fatalf("created job creator name %v", j.CreatedByUsername)
	}
	if got, err := s.GetJob(ctx, j.ID); err != nil || got.CreatedByUsername == nil || *got.CreatedByUsername != "dev" {
		t.Fatalf("get job creator name: %v %+v", err, got)
	}
	if list, err := s.ListJobs(ctx, JobFilter{}); err != nil || len(list) != 1 || list[0].CreatedByUsername == nil || *list[0].CreatedByUsername != "dev" {
		t.Fatalf("list job creator name: %v %+v", err, list)
	}
	if err := s.DeleteAccount(ctx, admin.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAPITokenByHash(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token survived account deletion: %v", err)
	}
	j2, err := s.GetJob(ctx, j.ID)
	if err != nil || j2.CreatedBy != nil || j2.CreatedByUsername != nil {
		t.Fatalf("job creator not nulled: %v %+v", err, j2)
	}
}

func TestAPITokenLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := mustAccount(t, s, "alice", RoleDeveloper, true)
	b := mustAccount(t, s, "bob", RoleDeveloper, true)
	exp := now.Add(time.Hour)
	tok, err := s.CreateAPIToken(ctx, NewAPIToken{AccountID: a.ID, Name: "ci", TokenHash: "hash-a", Prefix: "gdp_aaaa", ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	if !tok.CreatedAt.Equal(now) || tok.ExpiresAt == nil || !tok.ExpiresAt.Equal(exp) || tok.LastUsedAt != nil {
		t.Fatalf("unexpected token %+v", tok)
	}
	if _, err := s.CreateAPIToken(ctx, NewAPIToken{AccountID: a.ID, Name: "dup", TokenHash: "hash-a", Prefix: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate hash: %v", err)
	}
	if _, err := s.CreateAPIToken(ctx, NewAPIToken{AccountID: 999, Name: "x", TokenHash: "h", Prefix: "x"}); err == nil {
		t.Fatal("token for missing account accepted")
	}
	if tok.Expired(now) || !tok.Expired(exp) {
		t.Fatal("Expired boundary wrong")
	}

	// Touch writes at most once per interval.
	if err := s.TouchAPIToken(ctx, tok.ID, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchAPIToken(ctx, tok.ID, now.Add(30*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAPITokenByHash(ctx, "hash-a")
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(now) {
		t.Fatalf("last_used_at %v, want %v", got.LastUsedAt, now)
	}
	if err := s.TouchAPIToken(ctx, tok.ID, now.Add(2*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAPITokenByHash(ctx, "hash-a")
	if !got.LastUsedAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("last_used_at not advanced: %v", got.LastUsedAt)
	}

	list, err := s.ListAPITokens(ctx, a.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	// Another account cannot delete it.
	if err := s.DeleteAPIToken(ctx, b.ID, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account delete: %v", err)
	}
	if err := s.DeleteAPIToken(ctx, a.ID, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAPITokenByHash(ctx, "hash-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted token still found: %v", err)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	st, err := s.GetSettings(ctx)
	if err != nil || st.RegistrationEnabled {
		t.Fatalf("default settings %+v err %v", st, err)
	}
	on := true
	st, err = s.UpdateSettings(ctx, SettingsUpdate{RegistrationEnabled: &on})
	if err != nil || !st.RegistrationEnabled {
		t.Fatalf("update: %+v %v", st, err)
	}
	st, err = s.UpdateSettings(ctx, SettingsUpdate{})
	if err != nil || !st.RegistrationEnabled {
		t.Fatalf("empty update changed settings: %+v %v", st, err)
	}
	if _, ok, err := s.GetSetting(ctx, "nope"); ok || err != nil {
		t.Fatalf("missing key: %v %v", ok, err)
	}
}

func TestJobsAndReconcile(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	j1, err := s.CreateJob(ctx, NewJob{Kind: "project.pull", SubjectType: "project", SubjectID: "1"})
	if err != nil || j1.Status != JobQueued || j1.CreatedByUsername != nil {
		t.Fatalf("create: %v %+v", err, j1)
	}
	if err := s.MarkJobRunning(ctx, j1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkJobRunning(ctx, j1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double start: %v", err)
	}
	j2, _ := s.CreateJob(ctx, NewJob{Kind: "project.pull", SubjectType: "project", SubjectID: "2"})
	j3, _ := s.CreateJob(ctx, NewJob{Kind: "project.pull", SubjectType: "project", SubjectID: "1"})
	if err := s.FinishJob(ctx, j3.ID, JobSuccess, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, j3.ID, JobError, "again"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finishing a finished job: %v", err)
	}
	if n, _ := s.CountActiveJobs(ctx); n != 2 {
		t.Fatalf("active jobs %d, want 2", n)
	}
	list, err := s.ListJobs(ctx, JobFilter{SubjectType: "project", SubjectID: "1"})
	if err != nil || len(list) != 2 || list[0].ID != j3.ID {
		t.Fatalf("filtered list %+v err %v", list, err)
	}
	if list, _ := s.ListJobs(ctx, JobFilter{Limit: 1}); len(list) != 1 {
		t.Fatalf("limit ignored: %d", len(list))
	}

	p, err := s.CreateProject(ctx, Project{Name: "game", RepoURL: "https://example.com/g.git", Status: "pulling"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pIdle, _ := s.CreateProject(ctx, Project{Name: "other", RepoURL: "https://example.com/o.git", Status: "error"}, nil)

	res, err := s.Reconcile(ctx)
	if err != nil || res.Jobs != 2 || res.Projects != 1 {
		t.Fatalf("reconcile %+v err %v", res, err)
	}
	for _, id := range []int64{j1.ID, j2.ID} {
		j, _ := s.GetJob(ctx, id)
		if j.Status != JobError || j.Detail != InterruptedDetail || j.FinishedAt == nil {
			t.Fatalf("job %d not reconciled: %+v", id, j)
		}
	}
	if j, _ := s.GetJob(ctx, j3.ID); j.Status != JobSuccess {
		t.Fatalf("finished job touched: %+v", j)
	}
	if got, _ := s.GetProject(ctx, p.ID); got.Status != "error" {
		t.Fatalf("project status %q", got.Status)
	}
	if got, _ := s.GetProject(ctx, pIdle.ID); got.Status != "error" {
		t.Fatalf("non-transient project status changed to %q", got.Status)
	}
}

func TestProjectsCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p, err := s.CreateProject(ctx, Project{Name: "game", RepoURL: "https://x/g.git", Branch: "main", AuthType: "https", AuthUsername: "u"},
		func(id int64) ([]byte, error) { return []byte{byte(id), 0xAA}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "idle" || len(p.AuthSecret) != 2 || p.AuthSecret[0] != byte(p.ID) {
		t.Fatalf("unexpected project %+v", p)
	}
	if _, err := s.CreateProject(ctx, Project{Name: "game", RepoURL: "x"}, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	// A failing seal rolls back the insert.
	if _, err := s.CreateProject(ctx, Project{Name: "rollback", RepoURL: "x"}, func(int64) ([]byte, error) { return nil, errors.New("boom") }); err == nil {
		t.Fatal("seal failure ignored")
	}
	if _, err := s.GetProjectByName(ctx, "rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("insert not rolled back: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	p.LastCommit, p.LastPulledAt, p.AuthSecret, p.Status = "abc", &now, nil, "error"
	u, err := s.UpdateProject(ctx, p)
	if err != nil || u.LastCommit != "abc" || u.AuthSecret != nil || u.Status != "error" || !u.LastPulledAt.Equal(now) {
		t.Fatalf("update: %v %+v", err, u)
	}
	if err := s.SetProjectStatus(ctx, p.ID, "pulling"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountProjects(ctx); n != 1 {
		t.Fatalf("count %d", n)
	}
	if l, _ := s.ListProjects(ctx); len(l) != 1 {
		t.Fatalf("list %d", len(l))
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: %v", err)
	}
}

func TestServersCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p, _ := s.CreateProject(ctx, Project{Name: "game", RepoURL: "x"}, nil)
	b1, _ := s.CreateBuild(ctx, NewBuild{ProjectID: p.ID})
	b2, _ := s.CreateBuild(ctx, NewBuild{ProjectID: p.ID})
	port := 27015
	sv, err := s.CreateServer(ctx, Server{Name: "eu-1", BuildID: &b1.ID, Port: &port, AllowedOrigins: []string{"https://a.example"}},
		func(id int64) ([]byte, error) { return []byte("ct"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if sv.DesiredState != "stopped" || string(sv.Env) != "ct" || *sv.Port != port || *sv.BuildID != b1.ID ||
		len(sv.AllowedOrigins) != 1 || sv.AdminPort != 0 || sv.AdminToken != nil {
		t.Fatalf("unexpected server %+v", sv)
	}
	if _, err := s.CreateServer(ctx, Server{Name: "eu-2", Port: &port}, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate port: %v", err)
	}
	if _, err := s.CreateServer(ctx, Server{Name: "eu-1"}, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := s.CreateServer(ctx, Server{Name: "bad", DesiredState: "exploded"}, nil); err == nil {
		t.Fatal("invalid desired_state accepted")
	}
	missing := int64(999)
	if _, err := s.CreateServer(ctx, Server{Name: "nobuild", BuildID: &missing}, nil); err == nil {
		t.Fatal("unknown build accepted")
	}
	port2 := 27016
	u, err := s.UpdateServer(ctx, sv.ID, ServerUpdate{BuildID: &b2.ID, Port: &port2, SetOrigins: true})
	if err != nil || *u.BuildID != b2.ID || u.DesiredState != "stopped" || *u.Port != port2 || string(u.Env) != "ct" || len(u.AllowedOrigins) != 0 {
		t.Fatalf("update: %v %+v", err, u)
	}
	// The container record carries the admin endpoint and is cleared with it.
	if err := s.SetServerContainer(ctx, sv.ID, "c1", 41000, []byte("tok")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetServerAdminPort(ctx, sv.ID, "other", 42000); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetServer(ctx, sv.ID); got.ContainerID != "c1" || got.AdminPort != 41000 || string(got.AdminToken) != "tok" {
		t.Fatalf("%+v", got)
	}
	if err := s.SetServerAdminPort(ctx, sv.ID, "c1", 42000); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearServerContainer(ctx, sv.ID, "c1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetServer(ctx, sv.ID); got.ContainerID != "" || got.AdminPort != 0 || got.AdminToken != nil {
		t.Fatalf("%+v", got)
	}
	// A build a server references cannot be deleted.
	if err := s.DeleteBuild(ctx, b2.ID); err == nil {
		t.Fatal("referenced build deleted")
	}
	if n, _ := s.CountServers(ctx); n != 1 {
		t.Fatalf("count %d", n)
	}
	if l, _ := s.ListServers(ctx); len(l) != 1 {
		t.Fatalf("list %d", len(l))
	}
	if err := s.DeleteServer(ctx, sv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetServer(ctx, sv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
}

func TestLatestSuccessfulBuild(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p, _ := s.CreateProject(ctx, Project{Name: "game", RepoURL: "x"}, nil)
	other, _ := s.CreateProject(ctx, Project{Name: "other", RepoURL: "x"}, nil)
	mk := func(pid int64, commit, status, image string) Build {
		t.Helper()
		b, err := s.CreateBuild(ctx, NewBuild{ProjectID: pid})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MarkBuildRunning(ctx, b.ID, "main", commit); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishBuild(ctx, b.ID, BuildResult{Status: status, ImageID: image}); err != nil {
			t.Fatal(err)
		}
		return b
	}
	a1 := mk(p.ID, "aaa", BuildStatusSuccess, "sha256:1")
	a2 := mk(p.ID, "aaa", BuildStatusSuccess, "sha256:2")
	mk(p.ID, "aaa", BuildStatusError, "")
	bb := mk(p.ID, "bbb", BuildStatusSuccess, "sha256:3")
	mk(p.ID, "ccc", BuildStatusError, "")
	mk(other.ID, "aaa", BuildStatusSuccess, "sha256:4")
	if got, err := s.LatestSuccessfulBuild(ctx, p.ID, "aaa"); err != nil || got.ID != a2.ID {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := s.LatestSuccessfulBuild(ctx, p.ID, ""); err != nil || got.ID != bb.ID {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.LatestSuccessfulBuild(ctx, p.ID, "ccc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	imgs, err := s.BuildsByImage(ctx)
	if err != nil || len(imgs) != 4 || imgs["sha256:1"][0] != a1.ID {
		t.Fatalf("%v %v", imgs, err)
	}
}
