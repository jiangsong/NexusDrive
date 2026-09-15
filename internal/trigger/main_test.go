package trigger

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// TestMain doubles as the child process the exec tests run: the test
// binary re-executes itself with one of the modes below as argv[1]. The
// runner hands the child a scrubbed environment, so the mode travels in
// argv rather than in a GO_WANT_HELPER_PROCESS variable.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-echo-argv":
			// One element per line, so a test can prove an argument
			// reached the child whole, spaces and semicolons included.
			for _, a := range os.Args[2:] {
				fmt.Println(a)
			}
			os.Exit(0)
		case "-echo-env":
			env := os.Environ()
			sort.Strings(env)
			for _, kv := range env {
				fmt.Println(kv)
			}
			os.Exit(0)
		case "-exit":
			code, _ := strconv.Atoi(os.Args[2])
			fmt.Fprintln(os.Stderr, "failing on purpose")
			os.Exit(code)
		case "-sleep":
			d, _ := time.ParseDuration(os.Args[2])
			fmt.Println("sleeping")
			time.Sleep(d)
			os.Exit(0)
		case "-spew":
			// Print more than the output cap on both streams.
			n, _ := strconv.Atoi(os.Args[2])
			line := make([]byte, 1023)
			for i := range line {
				line[i] = 'x'
			}
			line = append(line, '\n')
			for i := 0; i < n; i++ {
				os.Stdout.Write(line)
				os.Stderr.Write(line)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// self is the program the exec tests run: this test binary.
func self() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return p
}
