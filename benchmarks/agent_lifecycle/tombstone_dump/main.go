// tombstone_dump opens a graymatter store read-only and prints every fact
// with its superseded_by field — the forensic half of the lifecycle finding.
package main

import (
	"fmt"
	"os"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func main() {
	dir := os.Args[1]
	s, err := memory.Open(memory.StoreConfig{DataDir: dir, ReadOnly: true})
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer s.Close()
	facts, err := s.List("phoenix-coder")
	if err != nil {
		fmt.Println("list:", err)
		os.Exit(1)
	}
	for _, f := range facts {
		if len(f.Text) > 60 {
			f.Text = f.Text[:60]
		}
		fmt.Printf("superseded_by=%q pinned=%v text=%q\n", f.SupersededBy, f.Pinned, f.Text)
	}
	fmt.Printf("total: %d facts\n", len(facts))
}
