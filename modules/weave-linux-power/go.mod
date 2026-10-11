module github.com/weaveplatform/weaveplatform-agent-modules/modules/weave-linux-power

go 1.27.2

require github.com/weaveplatform/weaveplatform-agent-modules/sdk v0.2.8

require (
	github.com/Microsoft/go-winio v0.6.3 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261005182115-fad411399dd8 // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/weaveplatform/weaveplatform-agent-modules/sdk => ../../sdk
