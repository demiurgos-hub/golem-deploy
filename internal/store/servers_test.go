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
	if _, err := s.CreateServer(ctx, Server{Name: "existing", Image: "img", Port: &p1}, nil); err != nil {
		t.Fatal(err)
	}
	// The second row collides on port: nothing is created.
	_, err := s.CreateServers(ctx, []Server{{Name: "a", Image: "img", Port: &p2}, {Name: "b", Image: "img", Port: &p1}}, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v", err)
	}
	if n, _ := s.CountServers(ctx); n != 1 {
		t.Fatalf("partial import: %d servers", n)
	}
	// A failing seal rolls back too.
	boom := errors.New("boom")
	_, err = s.CreateServers(ctx, []Server{{Name: "a", Image: "img", Port: &p2}}, func(int, int64) ([]byte, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	created, err := s.CreateServers(ctx, []Server{{Name: "a", Image: "img", Port: &p2}}, func(i int, id int64) ([]byte, error) { return []byte{byte(i), byte(id)}, nil })
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
	sv, err := s.CreateServer(ctx, Server{Name: "a", Image: "img", Port: &p}, func(int64) ([]byte, error) { return []byte("env"), nil })
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
