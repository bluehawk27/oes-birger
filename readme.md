# grpc-bidir

This sample implements a bi-directional event stream using GRPC.

As a warning, this is my first attempt at any Go code...

# Prerequisites

`go get -u github.com/golang/protobuf/{proto,protoc-gen-go}`
`go get -u google.golang.org/grpc`

# Building

`protoc --go_out=plugins=grpc:tunnel tunnel/tunnel.proto`

# Running

Start the server:
`go run server/server.go`

Start a client:
`go run client/client.go -identity skan1`

Send a request:
`go run sender/sender.go -identity skan1`
