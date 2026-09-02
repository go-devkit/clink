// Package urfave adapts a urfave/cli v3 command tree to a clink daemon.
//
// Every clink consumer using urfave/cli otherwise writes the same file: build a
// fresh command tree per session, point its I/O at the session, neutralise
// urfave's process-global exit and error sinks so a bad command cannot kill the
// daemon or print into its console, prepend argv[0], translate [cli.ExitCoder]
// into [clink.ExitError], and recover panics. None of that is
// application-specific, and getting any of it wrong fails quietly rather than
// loudly.
//
// The consumer keeps only what is genuinely theirs — building the tree:
//
//	return urfave.Serve(ctx, conf, func(s clink.Session) *cli.Command {
//	    return rootCommand(s, commands)
//	})
//
// This is a separate Go module: a subpackage would force urfave/cli onto every
// clink consumer, including those using cobra or no framework at all.
package urfave
