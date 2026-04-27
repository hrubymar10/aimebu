package server

import (
	"crypto/rand"
	"encoding/binary"
)

// namePool is the set of random English names used to identify AI agents.
// Kept short and unambiguous; avoid names that are also common words.
var namePool = []string{
	"alice", "bob", "carol", "dave", "eve", "frank", "grace", "hugo",
	"iris", "jack", "kate", "leo", "mia", "nate", "olive", "pete",
	"quinn", "ruth", "sam", "tina", "ulf", "vera", "will", "xena",
	"yara", "zeke", "amy", "ben", "cora", "dan", "ella", "finn",
	"gina", "hal", "ivy", "jade", "kai", "lena", "max", "nora",
	"otto", "piper", "rhea", "seth", "theo", "una", "vince", "wren",
	"xavi", "yuki", "zoe", "arlo", "beth", "cleo", "drew", "elsa",
	"felix", "gus", "hana", "ian", "june", "kira", "luna", "milo",
	"nina", "owen", "penny", "rex", "sage", "tara", "uma", "vlad",
	"wade", "xio", "yann", "zane", "ada", "boris", "cass", "deon",
	"emma", "ford", "gwen", "hans", "ines", "jonas", "kyra", "lars",
	"mona", "nils", "opal", "pia", "quin", "remi", "sven", "tessa",
	"umar", "viv", "wes", "xan", "yael", "zeno",
}

// pickRandomName returns a name from the pool that is not currently in
// `taken`. Returns the chosen name and true, or ("", false) if every name
// in the pool is taken.
func pickRandomName(taken map[string]bool) (string, bool) {
	// Random starting offset, then linear scan so we try every pool entry
	// at most once.
	var r [8]byte
	_, _ = rand.Read(r[:])
	start := int(binary.LittleEndian.Uint64(r[:]) % uint64(len(namePool)))
	for i := 0; i < len(namePool); i++ {
		name := namePool[(start+i)%len(namePool)]
		if !taken[name] {
			return name, true
		}
	}
	return "", false
}

// assembleID builds an agent ID per the project spec:
//
//	human:  martin
//	ai:     alice@aimebu   (model/harness stored in Agent struct, not in ID)
//	        alice          (no project)
func assembleID(kind, name, _, _, project string) string {
	if kind == "human" {
		return name
	}
	if project != "" {
		return name + "@" + project
	}
	return name
}
