module github.com/weaveplatform/weaveplatform-agent-modules/modules/weave-windows-display

go 1.27

require (
	github.com/deploymenttheory/go-bindings-win32 v0.5.0
	github.com/weaveplatform/weaveplatform-agent-modules/sdk v0.2.8
)

require (
	github.com/Microsoft/go-winio v0.6.4 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261026165753-2ffab32bce7e // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/weaveplatform/weaveplatform-agent-modules/sdk => ../../sdk
