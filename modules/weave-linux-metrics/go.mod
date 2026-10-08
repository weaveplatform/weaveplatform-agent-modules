module github.com/weaveplatform/weaveplatform-agent-modules/modules/weave-linux-metrics

go 1.27

require (
	github.com/weaveplatform/weaveplatform-agent-modules/sdk v0.2.5
	golang.org/x/sys v0.48.0
)

require (
	github.com/Microsoft/go-winio v0.6.3 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261005182115-fad411399dd8 // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/weaveplatform/weaveplatform-agent-modules/sdk => ../../sdk
