package dev

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
)

func TestSessionCatch(t *testing.T) {
	if mode := os.Getenv("GO_DEV_TEST_CATCH"); mode != "" {
		session := New()
		task := session.NewTask(Cmd{})
		task.OnClose(func() { fmt.Fprintln(os.Stdout, "closed") })
		if mode == "close-error" {
			task.listener = catchErrorListener{}
		}
		if !session.add(task) {
			t.Fatal("add task to open session")
		}
		var err error
		if mode != "nil" {
			err = errors.New("catch failed")
		}
		session.Catch(err)
		fmt.Fprintln(os.Stdout, "returned")
		os.Exit(0)
	}

	for _, test := range []struct {
		mode   string
		status int
		output string
	}{
		{mode: "nil", status: 0, output: "returned\n"},
		{mode: "error", status: 1, output: "closed\ncatch failed\n"},
		{mode: "close-error", status: 1, output: "closed\ncatch failed\nclose failed\n"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestSessionCatch$")
			command.Env = append(os.Environ(), Pair("GO_DEV_TEST_CATCH", test.mode))
			output, err := command.CombinedOutput()
			status := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatal(err)
				}
				status = exitErr.ExitCode()
			}
			if status != test.status {
				t.Fatalf("exit status = %d, want %d; output: %s", status, test.status, output)
			}
			if string(output) != test.output {
				t.Fatalf("output = %q, want %q", output, test.output)
			}
		})
	}
}

type catchErrorListener struct{ net.Listener }

func (catchErrorListener) Close() error { return errors.New("close failed") }
