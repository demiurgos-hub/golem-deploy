package store

import (
	"context"
	"errors"
	"testing"
)

func TestCreateServersIsAtomic(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p1, p2 := 7001, 7002
	if _, err := s.CreateServer(ctx, Server{Name: "existing", Port: &p1}, nil); err != nil {
		t.Fatal(err)
	}
	// The second row collides on port: nothing is created.
	_, err := s.CreateServers(ctx, []Server{{Name: "a", Port: &p2}, {Name: "b", Port: &p1}}, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v", err)
	}
	if n, _ := s.CountServers(ctx); n != 1 {
		t.Fatalf("partial import: %d servers", n)
	}
	// A failing seal rolls back too.
	boom := errors.New("boom")
	_, err = s.CreateServers(ctx, []Server{{Name: "a", Port: &p2}}, func(int, int64) ([]byte, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	created, err := s.CreateServers(ctx, []Server{{Name: "a", Port: &p2}}, func(i int, id int64) ([]byte, error) { return []byte{byte(i), byte(id)}, nil })
	if err != nil || len(created) != 1 || created[0].DesiredState != "stopped" || len(created[0].Env) != 2 {
		t.Fatalf("%+v %v", created, err)
	}
	got, _ := s.GetServer(ctx, created[0].ID)
	if string(got.Env) != string(created[0].Env) {
		t.Fatal("env not stored")
	}
}

func TestSetServerDesiredState(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := 7001
	sv, err := s.CreateServer(ctx, Server{Name: "a", Port: &p}, func(int64) ([]byte, error) { return []byte("env"), nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"running", "running", "stopped"} {
		if err := s.SetServerDesiredState(ctx, sv.ID, st); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetServer(ctx, sv.ID)
		if got.DesiredState != st || string(got.Env) != "env" {
			t.Fatalf("%+v", got)
		}
	}
	if err := s.SetServerDesiredState(ctx, 999, "running"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := s.SetServerDesiredState(ctx, sv.ID, "bogus"); err == nil {
		t.Fatal("invalid state accepted")
	}
}

// The Inspector switch defaults to off, is set at create and changed only
// by an update that names it; the database refuses other values.
func TestServerInspectorSwitch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p1, p2 := 7001, 7002
	plain, err := s.CreateServer(ctx, Server{Name: "plain", Port: &p1}, nil)
	if err != nil || plain.Inspector {
		t.Fatalf("%+v %v", plain, err)
	}
	on, err := s.CreateServer(ctx, Server{Name: "on", Port: &p2, Inspector: true}, nil)
	if err != nil || !on.Inspector {
		t.Fatalf("%+v %v", on, err)
	}
	yes, no := true, false
	if got, err := s.UpdateServer(ctx, plain.ID, ServerUpdate{Inspector: &yes}); err != nil || !got.Inspector {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := s.UpdateServer(ctx, plain.ID, ServerUpdate{SetOrigins: true}); err != nil || !got.Inspector {
		t.Fatalf("an update without the switch changed it: %+v %v", got, err)
	}
	if got, err := s.UpdateServer(ctx, on.ID, ServerUpdate{Inspector: &no}); err != nil || got.Inspector {
		t.Fatalf("%+v %v", got, err)
	}
	batch, err := s.CreateServers(ctx, []Server{{Name: "imported", Inspector: true}}, nil)
	if err != nil || !batch[0].Inspector {
		t.Fatalf("%+v %v", batch, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE servers SET inspector = 2 WHERE id = ?", on.ID); err == nil {
		t.Fatal("inspector = 2 accepted")
	}
}

// The Profiling switch is written at create and changed only by an update
// that names it; rows that existed before the column (and any insert that
// leaves it out) are on, and the database refuses other values.
func TestServerProfilingSwitch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p1, p2 := 7001, 7002
	off, err := s.CreateServer(ctx, Server{Name: "off", Port: &p1}, nil)
	if err != nil || off.Profiling {
		t.Fatalf("%+v %v", off, err)
	}
	on, err := s.CreateServer(ctx, Server{Name: "on", Port: &p2, Profiling: true}, nil)
	if err != nil || !on.Profiling {
		t.Fatalf("%+v %v", on, err)
	}
	yes, no := true, false
	if got, err := s.UpdateServer(ctx, off.ID, ServerUpdate{Profiling: &yes}); err != nil || !got.Profiling {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := s.UpdateServer(ctx, off.ID, ServerUpdate{SetOrigins: true}); err != nil || !got.Profiling {
		t.Fatalf("an update without the switch changed it: %+v %v", got, err)
	}
	if got, err := s.UpdateServer(ctx, on.ID, ServerUpdate{Profiling: &no}); err != nil || got.Profiling {
		t.Fatalf("%+v %v", got, err)
	}
	batch, err := s.CreateServers(ctx, []Server{{Name: "imported", Profiling: true}}, nil)
	if err != nil || !batch[0].Profiling {
		t.Fatalf("%+v %v", batch, err)
	}
	var def int
	if err := s.db.QueryRowContext(ctx, "SELECT dflt_value FROM pragma_table_info('servers') WHERE name = 'profiling'").Scan(&def); err != nil || def != 1 {
		t.Fatalf("profiling column default %d %v, want 1", def, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE servers SET profiling = 2 WHERE id = ?", on.ID); err == nil {
		t.Fatal("profiling = 2 accepted")
	}
}

// The Inspector settings are plain rows of the settings table.
func TestInspectorSettings(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, ok, err := s.GetSetting(ctx, SettingInspectorEnabled); err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
	for _, k := range []string{SettingInspectorEnabled, SettingInspectorSecrets} {
		if err := s.SetSetting(ctx, k, "v1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSetting(ctx, k, "v2"); err != nil {
			t.Fatal(err)
		}
		if v, ok, err := s.GetSetting(ctx, k); err != nil || !ok || v != "v2" {
			t.Fatalf("%s: %q %v %v", k, v, ok, err)
		}
	}
}
