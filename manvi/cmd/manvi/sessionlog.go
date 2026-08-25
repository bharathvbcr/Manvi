package main

import (
	"manvi/credentials"
	"manvi/session"
)

// newSessionLog builds a session log with the credential backstop already
// armed.
//
// It exists so that the session package's raw constructor is called exactly
// once in this package.
// The log is two boundaries at once — what session.Store writes to disk, and
// what DeriveMessages projects into the next request — so a log built without
// the backstop is a log that can put a credential in a file and then send it to
// a provider on the following turn. There were three construction sites, which
// is three chances to forget; a helper is one.
//
// TestSessionLogsAreArmedAtOneSite is what keeps that true rather than merely
// intended: it reads this package's own source and fails if the raw
// constructor is called anywhere else.
func newSessionLog() *session.Log {
	log := session.NewLog()
	scrubber := credentials.NewScrubber()
	scrubber.WatchAll(credentials.NewResolver())
	log.SetScrubber(scrubber.Clean)
	return log
}
