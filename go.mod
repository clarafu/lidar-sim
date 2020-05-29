module github.com/clarafu/concourse-lidar-simulator

go 1.14

replace github.com/concourse/concourse => /Users/clarafu/workspace/concourse

require (
	code.cloudfoundry.org/clock v1.0.0
	code.cloudfoundry.org/lager v2.0.0+incompatible
	github.com/bmatcuk/doublestar v1.3.0 // indirect
	github.com/cloudfoundry/bosh-cli v6.2.1+incompatible // indirect
	github.com/cloudfoundry/bosh-utils v0.0.0-20200516100215-fa15a6f68ca0 // indirect
	github.com/concourse/atc v4.2.2+incompatible
	github.com/concourse/concourse v1.6.1-0.20200521012016-fef8844fbcd1
	github.com/cppforlife/go-patch v0.2.0 // indirect
	github.com/onsi/ginkgo v1.12.2 // indirect
	github.com/tedsuo/ifrit v0.0.0-20180802180643-bea94bb476cc
	go.opentelemetry.io/otel/exporter/trace/jaeger v0.2.1
)
