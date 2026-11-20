// Package exampletest runs example commands against temporary standard streams.
package exampletest

import (
	"io"
	"os"
	"testing"
)

// RunCLI starts main in CLI mode, supplies input through stdin and returns stdout.
func RunCLI(t *testing.T, main func(), input string) string {
	t.Helper()
	t.Chdir(t.TempDir()) // Isolate .env files and local attachments.
	in, err := os.CreateTemp(".", "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := in.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(".", "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	stdin, stdout, args := os.Stdin, os.Stdout, os.Args
	defer func() { os.Stdin, os.Stdout, os.Args = stdin, stdout, args }()
	os.Stdin, os.Stdout, os.Args = in, out, []string{"example", "cli"}
	main()
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
