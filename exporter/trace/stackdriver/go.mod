module go.opentelemetry.io/exporter/trace/stackdriver

go 1.13

replace go.opentelemetry.io => ../../..

require (
	cloud.google.com/go v0.47.0
	github.com/golang/protobuf v1.3.2
	go.opentelemetry.io v0.0.0-20191031063502-886243699327
	golang.org/x/oauth2 v0.0.0-20190604053449-0f29369cfe45
	google.golang.org/api v0.11.0
	google.golang.org/genproto v0.0.0-20191009194640-548a555dbc03
	google.golang.org/grpc v1.24.0
)
