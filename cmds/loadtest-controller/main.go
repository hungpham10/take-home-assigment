// Command loadtest-controller is the control plane BFF for the load testing
// platform: namespace/component CRUD, OpenAPI validation, script/loadtest
// lifecycle and RBAC. Implementation lands incrementally per plans/08..13.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "loadtest-controller: not implemented yet (see plans/08-controller-bootstrap.md)")
	os.Exit(1)
}
