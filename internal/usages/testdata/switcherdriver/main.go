package main

import (
	"fmt"
	"os"

	"github.com/hrubymar10/aimebu/internal/switcher"
)

func main() {
	if len(os.Args) != 5 {
		panic("usage: switcherdriver <setup|switch|switchmain|rename> <root> <home> <claude|codex>")
	}
	m, err := switcher.New(os.Args[2])
	if err != nil {
		panic(err)
	}
	m.SetHomeDir(os.Args[3])
	switch os.Args[1] {
	case "setup":
		err = m.SetEnabled(true)
		if err == nil {
			err = m.Import(os.Args[4], "main")
		}
		if err == nil {
			err = m.Add(os.Args[4], "other")
		}
	case "switch":
		_, _, err = m.Switch(os.Args[4], "other")
	case "switchmain":
		_, _, err = m.Switch(os.Args[4], "main")
	case "rename":
		err = m.Rename(os.Args[4], "main", "renamed")
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		panic(err)
	}
}
