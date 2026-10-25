package service

import (
	"errors"
	"strings"
	"testing"
)

func TestSleepWakeState(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if args[0] == "is-active" {
			return "inactive", errors.New("exit status 3")
		}
		return "", nil
	}
	if err := Sleep(run); err != nil {
		t.Fatal(err)
	}
	if err := Wake(run); err != nil {
		t.Fatal(err)
	}
	if s := State(run); s != "inactive" {
		t.Fatalf("state = %q", s)
	}
	want := []string{
		"systemctl disable --now tigersb.service",
		"systemctl enable --now tigersb.service",
		"systemctl is-active tigersb.service",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
}

func TestSleepError(t *testing.T) {
	run := func(string, ...string) (string, error) { return "Access denied", errors.New("exit status 1") }
	if err := Sleep(run); err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("expected error with output, got %v", err)
	}
}
