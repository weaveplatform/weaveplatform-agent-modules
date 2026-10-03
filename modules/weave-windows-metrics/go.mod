module github.com/weaveplatform/weaveplatform-agent-modules/modules/weave-windows-metrics

go 1.27

require (
	github.com/deploymenttheory/go-bindings-win32 v0.5.0
	github.com/weaveplatform/weaveplatform-agent-modules/sdk v0.0.0-00010101000000-000000000000
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/weaveplatform/weaveplatform-agent-modules/sdk => ../../sdk
