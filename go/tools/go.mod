module github.com/Teamtem-dev/kuben/go/tools

go 1.27.0

require (
	github.com/alecthomas/go-check-sumtype v0.5.0 // indirect
	github.com/kisielk/errcheck v1.20.0 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/nishanths/exhaustive v0.13.0 // indirect
	go.uber.org/nilaway v0.0.0-20260918162853-acb8859b9031 // indirect
	golang.org/x/exp/typeparams v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/telemetry v0.0.0-20260908163034-4bcc4b2ee518 // indirect
	golang.org/x/tools v0.50.0 // indirect
	golang.org/x/vuln v1.8.0 // indirect
	mvdan.cc/gofumpt v0.12.0 // indirect
)

tool (
	github.com/alecthomas/go-check-sumtype/cmd/go-check-sumtype
	github.com/kisielk/errcheck
	github.com/nishanths/exhaustive/cmd/exhaustive
	go.uber.org/nilaway/cmd/nilaway
	golang.org/x/vuln/cmd/govulncheck
	mvdan.cc/gofumpt
)
