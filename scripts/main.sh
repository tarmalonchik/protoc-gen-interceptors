#!/usr/bin/env bash

BUF=$(which buf)
export BUF

GO=$(which go)
export GO

generate() {
  ${BUF} generate
  ${BUF} generate --template buf.gen.postprocess.yaml
}

dependencies() {
  $GO install \
  github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway \
  google.golang.org/grpc/cmd/protoc-gen-go-grpc \
  google.golang.org/protobuf/cmd/protoc-gen-go \
  github.com/tarmalonchik/protoc-gen-interceptors
}

"$@"