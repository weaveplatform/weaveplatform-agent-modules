// Module tools pins the developer tools the Makefile and CI run with
// `go tool -modfile=tools/go.mod`, so deps-refresh keeps them at latest like
// any other dependency. It is not in go.work and holds no code.
module github.com/weaveplatform/weaveplatform-agent-modules/tools

go 1.27

tool (
	github.com/vladopajic/go-test-coverage/v2
	golang.org/x/vuln/cmd/govulncheck
)

require (
	github.com/alexflint/go-arg v1.6.1 // indirect
	github.com/alexflint/go-scalar v1.2.0 // indirect
	github.com/aws/aws-sdk-go-v2 v1.48.2 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.29 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.21.5 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.14 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.14 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.14 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.27 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.15 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.14 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.14 // indirect
	github.com/aws/aws-sdk-go-v2/service/s3 v1.116.2 // indirect
	github.com/aws/smithy-go v1.28.11 // indirect
	github.com/golang/freetype v0.0.0-20170609003504-e2365dfdc4a0 // indirect
	github.com/google/go-github/v88 v88.0.0 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-colorable v0.1.16 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/narqo/go-badge v0.0.0-20230821190521-c9a75c019a59 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	github.com/vladopajic/go-test-coverage/v2 v2.23.0 // indirect
	golang.org/x/image v0.48.0 // indirect
	golang.org/x/mod v0.43.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/telemetry v0.0.0-20261124234621-1402bdb29cb5 // indirect
	golang.org/x/tools v0.53.0 // indirect
	golang.org/x/vuln v1.10.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
