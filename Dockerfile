FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o /out/agent-loop ./cmd/agent-loop

FROM alpine:3.22 AS app
RUN apk add --no-cache ca-certificates
COPY --from=build /out/agent-loop /usr/local/bin/agent-loop
EXPOSE 8080
ENTRYPOINT ["agent-loop"]

FROM build AS test
CMD ["go", "test", "-tags=rest", "-count=1", "./api"]
