package ports

import (
	"errors"
	"net"
	"strconv"
	"testing"
)

func TestParseAndValidate(t *testing.T) {
	cases := []struct {
		in    string
		panel int
		ok    bool
	}{
		{"7000-7099", 8000, true},
		{"7000", 0, true},
		{" 20000 - 20010 ", 0, true},
		{"6000-6100", 0, true}, // unsafe ports inside are skipped, not fatal
		{"7099-7000", 0, false},
		{"80-90", 0, false},
		{"1000-1100", 0, false},
		{"30000-33000", 0, false},
		{"32768-32800", 0, false},
		{"7000-8100", 8000, false},
		{"6665-6669", 0, false}, // nothing usable
		{"abc", 0, false},
		{"7000-", 0, false},
	}
	for _, c := range cases {
		r, err := Parse(c.in)
		if err == nil {
			err = r.Validate(c.panel)
		}
		if (err == nil) != c.ok {
			t.Errorf("%q panel %d: err=%v, want ok=%v", c.in, c.panel, err, c.ok)
		}
	}
}

func TestLowestSkipsUsedAndUnsafe(t *testing.T) {
	r := Range{Lo: 5999, Hi: 6002}
	p, err := r.Lowest(map[int]bool{5999: true})
	if err != nil || p != 6001 {
		t.Fatalf("got %d %v, want 6001 (6000 is unsafe)", p, err)
	}
	if r.Allowed(6000) || !r.Allowed(6002) || r.Allowed(6003) {
		t.Fatal("Allowed wrong")
	}
	if _, err := r.Lowest(map[int]bool{5999: true, 6001: true, 6002: true}); !errors.Is(err, ErrExhausted) {
		t.Fatalf("got %v", err)
	}
}

func TestProbeFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	if err := ProbeFree("127.0.0.1", busy); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy tcp port probed free: %v", err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	udp := pc.LocalAddr().(*net.UDPAddr).Port
	if err := ProbeFree("127.0.0.1", udp); err != nil {
		// The TCP side may be free; UDP must be reported busy.
		if !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	} else {
		t.Fatalf("busy udp port %s probed free", strconv.Itoa(udp))
	}
}
