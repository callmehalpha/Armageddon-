// Command fakecodeserver stands in for code-server in the e2e tests, so CI
// does not need the real thing.
package main

import (
	"os"

	"github.com/callmehalpha/Armageddon-/internal/testutil/fakecodeserver"
)

func main() { os.Exit(fakecodeserver.Main(os.Args[1:])) }
