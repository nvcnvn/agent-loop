# Tech Debts

## OpenTelemetry held at v1.38.0

`go.opentelemetry.io/otel` and its `trace`, `sdk` and `otlptracehttp` modules stay at v1.38.0
because later releases, through their `golang.org/x` dependencies, need Go 1.26, and `go.mod` and
CI target Go 1.25.

Remove when the project's Go floor is 1.26: upgrade the four modules together and run `go mod tidy`.
