package main

import (
	"encoding/json"
	"fmt"
	"os"

	"OpenLinkHub/src/icueprofile"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <profile.cueprofile>\n", os.Args[0])
		os.Exit(2)
	}
	p, err := icueprofile.ParseFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "iCUE profile parse failed:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
