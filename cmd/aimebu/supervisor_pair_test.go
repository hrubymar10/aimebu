package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteMachineKeyPrivateAndNotReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine.key")
	first := hostMachineCredential{MachineID: "mch_first", Key: "amk_first", ServerURL: "https://example.test"}
	if err := writeRemoteMachineKey(path, first); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("machine key mode = %o", info.Mode().Perm())
	}
	if err := writeRemoteMachineKey(path, hostMachineCredential{MachineID: "mch_second", Key: "amk_second"}); err == nil {
		t.Fatal("existing machine key replaced")
	}
	got, err := readHostMachineKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatalf("machine key changed: %+v", got)
	}
}
