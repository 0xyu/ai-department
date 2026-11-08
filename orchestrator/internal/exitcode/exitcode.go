// Package exitcode defines the process exit codes of the aco binary.
package exitcode

// Exit codes. They are part of the public CLI contract.
const (
	Completed       = 0 // run completed / command succeeded
	Failed          = 1 // run failed (system problem)
	Usage           = 2 // usage or configuration error, or command not implemented
	PreflightFailed = 3 // preflight / doctor checks failed
	Cancelled       = 4 // run cancelled
	Blocked         = 5 // run blocked (needs a human decision)
)
