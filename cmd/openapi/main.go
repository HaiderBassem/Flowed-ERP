// Command openapi writes the API contract from the live router.
//
// Run by `make openapi`, and its output is checked in so clients can be
// generated from a file rather than from a running server. A test regenerates
// and compares, so the checked-in document cannot drift from what the router
// actually serves.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"flowed/internal/adapter/httpapi"
)

func main() {
	output := "api/openapi.json"
	if len(os.Args) > 1 {
		output = os.Args[1]
	}

	spec := httpapi.BuildSpec(httpapi.NewSpecRouter(), "generated")
	encoded, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapi: encoding: %v\n", err)
		os.Exit(1)
	}
	encoded = append(encoded, '\n')

	if err := os.WriteFile(output, encoded, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "openapi: writing %s: %v\n", output, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d paths)\n", output, len(spec.Paths))
}
