module github.com/bharathvbcr/Manvi/manvi

go 1.26.6

// The toolchain CI and releases build with. `go` above is the language floor;
// govulncheck under go1.26.6 reports ten standard-library advisories reachable
// from this module (net/http, net/textproto, crypto/tls, os), all fixed in
// 1.26.9, and none under it.
toolchain go1.26.10

require (
	github.com/awnumar/memguard v0.23.0
	github.com/bharathvbcr/DevCouncil/backend/go_orchestrator v0.0.0
	github.com/bharathvbcr/gusset v0.0.0
	github.com/quasilyte/go-ruleguard/dsl v0.3.23
	golang.org/x/sys v0.47.0
)

require (
	github.com/awnumar/memcall v0.4.0 // indirect
	golang.org/x/crypto v0.56.0 // indirect
)

replace github.com/bharathvbcr/DevCouncil/backend/go_orchestrator => ../../DevCouncil/backend/go_orchestrator

replace github.com/bharathvbcr/gusset => ../../gusset
